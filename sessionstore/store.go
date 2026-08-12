// Package sessionstore provides SQLite-backed persistence for agent chat
// sessions, message history, and prompt history.
package sessionstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"

	"github.com/ParthSareen/o/api"
)

// Store wraps a SQLite database storing chat sessions and messages.
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// SessionMeta is the lightweight metadata for a session, used in listings.
type SessionMeta struct {
	ID         string
	Name       string
	Model      string
	Title      string
	WorkingDir string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Session is a full session with its message history.
type Session struct {
	ID         string
	Name       string
	Model      string
	WorkingDir string
	System     string
	Title      string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Messages   []api.Message
}

// dbPath returns the path to the sessions database under ~/.o/.
func dbPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".o")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create ~/.o: %w", err)
	}
	return filepath.Join(dir, "sessions.db"), nil
}

// Open opens (or creates) the sessions database at ~/.o/sessions.db and
// runs schema migrations.
func Open() (*Store, error) {
	path, err := dbPath()
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open sessions db: %w", err)
	}
	// SQLite supports concurrent reads but only one writer. A single
	// connection from the pool is sufficient for this CLI's usage pattern.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(schemaSQL)
	if err != nil {
		return err
	}
	return s.addColumnIfMissing("sessions", "name", "TEXT NOT NULL DEFAULT ''")
}

// addColumnIfMissing adds a column to a table if it is not already present.
// This keeps existing databases compatible when the schema grows.
func (s *Store) addColumnIfMissing(table, column, definition string) error {
	rows, err := s.db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return fmt.Errorf("check %s columns: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, definition))
	if err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    model       TEXT NOT NULL,
    working_dir TEXT,
    system      TEXT,
    name        TEXT NOT NULL DEFAULT '',
    title       TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    role         TEXT NOT NULL,
    content      TEXT,
    thinking     TEXT,
    tool_calls   TEXT,
    tool_call_id TEXT,
    tool_name    TEXT,
    images       TEXT,
    created_at   INTEGER NOT NULL,
    UNIQUE(session_id, seq)
);

CREATE TABLE IF NOT EXISTS prompt_history (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id  TEXT,
    prompt      TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_prompts_session ON prompt_history(session_id, id DESC);
`

// CreateSession inserts a new session row and returns it. The name is a
// human-settable label (may be empty); title is auto-derived later.
func (s *Store) CreateSession(model, workingDir, system, name string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	id := uuid.NewString()
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, model, working_dir, system, name, title, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, model, workingDir, system, name, "", now.Unix(), now.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &Session{
		ID:         id,
		Name:       name,
		Model:      model,
		WorkingDir: workingDir,
		System:     system,
		CreatedAt:  now,
		UpdatedAt:  now,
	}, nil
}

// LoadSession loads a session's metadata and all messages ordered by seq.
func (s *Store) LoadSession(id string) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess := &Session{}
	var createdAt, updatedAt int64
	err := s.db.QueryRow(
		`SELECT id, model, working_dir, system, name, title, created_at, updated_at FROM sessions WHERE id = ?`,
		id,
	).Scan(&sess.ID, &sess.Model, &sess.WorkingDir, &sess.System, &sess.Name, &sess.Title, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("session %s not found", id)
		}
		return nil, fmt.Errorf("load session: %w", err)
	}
	sess.CreatedAt = time.Unix(createdAt, 0)
	sess.UpdatedAt = time.Unix(updatedAt, 0)

	rows, err := s.db.Query(
		`SELECT seq, role, content, thinking, tool_calls, tool_call_id, tool_name, images FROM messages WHERE session_id = ? ORDER BY seq`,
		id,
	)
	if err != nil {
		return nil, fmt.Errorf("load session messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		sess.Messages = append(sess.Messages, msg)
	}
	return sess, rows.Err()
}

// ListSessions returns session metadata ordered by most recently updated.
func (s *Store) ListSessions(limit int) ([]SessionMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT id, name, model, title, working_dir, created_at, updated_at FROM sessions ORDER BY updated_at DESC, rowid DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []SessionMeta
	for rows.Next() {
		var sm SessionMeta
		var createdAt, updatedAt int64
		if err := rows.Scan(&sm.ID, &sm.Name, &sm.Model, &sm.Title, &sm.WorkingDir, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		sm.CreatedAt = time.Unix(createdAt, 0)
		sm.UpdatedAt = time.Unix(updatedAt, 0)
		sessions = append(sessions, sm)
	}
	return sessions, rows.Err()
}

// MostRecentSession returns the most recently updated session, or nil if none.
func (s *Store) MostRecentSession() (*SessionMeta, error) {
	sessions, err := s.ListSessions(1)
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return nil, nil
	}
	return &sessions[0], nil
}

// DeleteSession removes a session and all its messages (cascade).
func (s *Store) DeleteSession(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// SetTitle updates the session title.
func (s *Store) SetTitle(sessionID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`, title, time.Now().Unix(), sessionID)
	return err
}

// SetName updates the human-settable session name.
func (s *Store) SetName(sessionID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE sessions SET name = ?, updated_at = ? WHERE id = ?`, name, time.Now().Unix(), sessionID)
	return err
}

// SetModel updates the stored model for a session. Used when a session is
// resumed against a different model so future plain resumes keep the choice.
func (s *Store) SetModel(sessionID, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE sessions SET model = ?, updated_at = ? WHERE id = ?`, model, time.Now().Unix(), sessionID)
	return err
}

// AppendMessages appends messages to a session starting at the current max
// seq + 1. It updates the session's updated_at timestamp. This is append-only:
// existing rows are never modified. If the session has no title, the first
// user message's content (truncated) is used.
func (s *Store) AppendMessages(sessionID string, msgs []api.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var maxSeq int
	err = tx.QueryRow(`SELECT COALESCE(MAX(seq), -1) FROM messages WHERE session_id = ?`, sessionID).Scan(&maxSeq)
	if err != nil {
		return fmt.Errorf("get max seq: %w", err)
	}

	now := time.Now().Unix()
	for _, msg := range msgs {
		maxSeq++
		if err := insertMessage(tx, sessionID, maxSeq, msg, now); err != nil {
			return err
		}
	}

	// Auto-set title from the first user message if title is empty.
	var title string
	err = tx.QueryRow(`SELECT title FROM sessions WHERE id = ?`, sessionID).Scan(&title)
	if err != nil {
		return fmt.Errorf("get title: %w", err)
	}
	if title == "" {
		for _, msg := range msgs {
			if msg.Role == "user" && msg.Content != "" {
				title = deriveTitle(msg.Content)
				break
			}
		}
		if title != "" {
			if _, err := tx.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, title, sessionID); err != nil {
				return fmt.Errorf("set title: %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}

	return tx.Commit()
}

// AddPrompt records a user prompt in the prompt history table. A nil
// sessionID stores it as global history.
func (s *Store) AddPrompt(sessionID, prompt string) error {
	prompt = trimPrompt(prompt)
	if prompt == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO prompt_history (session_id, prompt, created_at) VALUES (?, ?, ?)`,
		sessionID, prompt, time.Now().Unix(),
	)
	return err
}

// RecentPrompts returns up to limit most recent prompts for a session (or
// global if sessionID is empty), most recent first.
func (s *Store) RecentPrompts(sessionID string, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if sessionID != "" {
		rows, err = s.db.Query(
			`SELECT prompt FROM prompt_history WHERE session_id = ? OR session_id IS NULL ORDER BY id DESC LIMIT ?`,
			sessionID, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT prompt FROM prompt_history ORDER BY id DESC LIMIT ?`,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("recent prompts: %w", err)
	}
	defer rows.Close()

	var prompts []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// MessageCount returns the number of stored messages for a session.
func (s *Store) MessageCount(sessionID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&count)
	return count, err
}

// --- helpers ---

func insertMessage(tx *sql.Tx, sessionID string, seq int, msg api.Message, createdAt int64) error {
	var toolCallsJSON any
	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return fmt.Errorf("marshal tool calls: %w", err)
		}
		toolCallsJSON = string(b)
	}

	var imagesJSON any
	if len(msg.Images) > 0 {
		b, err := json.Marshal(msg.Images)
		if err != nil {
			return fmt.Errorf("marshal images: %w", err)
		}
		imagesJSON = string(b)
	}

	_, err := tx.Exec(
		`INSERT INTO messages (session_id, seq, role, content, thinking, tool_calls, tool_call_id, tool_name, images, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, seq, msg.Role, msg.Content, msg.Thinking, toolCallsJSON, msg.ToolCallID, msg.ToolName, imagesJSON, createdAt,
	)
	if err != nil {
		return fmt.Errorf("insert message seq %d: %w", seq, err)
	}
	return nil
}

// messageScanner is the minimal scan interface needed by scanMessage.
type messageScanner interface {
	Scan(dest ...any) error
}

func scanMessage(rows messageScanner) (api.Message, error) {
	var msg api.Message
	var toolCallsJSON, imagesJSON sql.NullString
	if err := rows.Scan(
		new(int), // seq — discarded, we rely on order
		&msg.Role,
		&msg.Content,
		&msg.Thinking,
		&toolCallsJSON,
		&msg.ToolCallID,
		&msg.ToolName,
		&imagesJSON,
	); err != nil {
		return api.Message{}, fmt.Errorf("scan message: %w", err)
	}
	if toolCallsJSON.Valid && toolCallsJSON.String != "" {
		if err := json.Unmarshal([]byte(toolCallsJSON.String), &msg.ToolCalls); err != nil {
			return api.Message{}, fmt.Errorf("unmarshal tool calls: %w", err)
		}
	}
	if imagesJSON.Valid && imagesJSON.String != "" {
		if err := json.Unmarshal([]byte(imagesJSON.String), &msg.Images); err != nil {
			return api.Message{}, fmt.Errorf("unmarshal images: %w", err)
		}
	}
	return msg, nil
}

func deriveTitle(content string) string {
	// First non-empty line, truncated to 80 runes, newlines replaced.
	title := content
	for _, line := range splitLines(content) {
		line = trimSpace(line)
		if line != "" {
			title = line
			break
		}
	}
	r := []rune(title)
	if len(r) > 80 {
		title = string(r[:77]) + "..."
	}
	return title
}

func trimPrompt(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i, c := range s {
		if c == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}
