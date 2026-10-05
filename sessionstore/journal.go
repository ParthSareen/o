package sessionstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ParthSareen/o/agent"
	"github.com/ParthSareen/o/api"
)

// The journal tables implement o's durable-execution boundary: committed
// runs, tool intent/outcome, an append-only raw record, compaction facts,
// approvals, and per-session run locks. All additions are new tables —
// older binaries ignore them, sessions/messages/prompt_history are
// untouched.
const journalSchemaSQL = `
CREATE TABLE IF NOT EXISTS runs (
    run_id       TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    request_id   TEXT NOT NULL DEFAULT '',
    attempt      INTEGER NOT NULL DEFAULT 1,
    input_hash   TEXT NOT NULL,
    prompt       TEXT NOT NULL,
    model        TEXT NOT NULL,
    system       TEXT,
    skill        TEXT NOT NULL DEFAULT '',
    format       TEXT NOT NULL DEFAULT '',
    options      TEXT NOT NULL DEFAULT '',
    think        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL,
    error        TEXT NOT NULL DEFAULT '',
    last_seq     INTEGER NOT NULL DEFAULT 0,
    admitted_at  INTEGER NOT NULL,
    terminal_at  INTEGER
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_request_attempt
    ON runs(session_id, request_id, attempt) WHERE request_id <> '';
CREATE INDEX IF NOT EXISTS idx_runs_session ON runs(session_id, admitted_at);

CREATE TABLE IF NOT EXISTS tool_calls (
    tool_call_id        TEXT NOT NULL,
    run_id              TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    session_id          TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    parent_tool_call_id TEXT NOT NULL DEFAULT '',
    tool_name           TEXT NOT NULL,
    args_hash           TEXT NOT NULL,
    args                TEXT NOT NULL DEFAULT '',
    replay_safe         INTEGER NOT NULL DEFAULT 0,
    status              TEXT NOT NULL DEFAULT 'pending',
    result              TEXT NOT NULL DEFAULT '',
    error               TEXT NOT NULL DEFAULT '',
    intent_seq          INTEGER NOT NULL,
    outcome_seq         INTEGER,
    created_at          INTEGER NOT NULL,
    settled_at          INTEGER,
    PRIMARY KEY (run_id, tool_call_id)
);

CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id, status);

CREATE TABLE IF NOT EXISTS session_events (
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq          INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    version      INTEGER NOT NULL DEFAULT 1,
    run_id       TEXT NOT NULL DEFAULT '',
    tool_call_id TEXT NOT NULL DEFAULT '',
    payload      TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (session_id, seq)
);

CREATE TABLE IF NOT EXISTS raw_history (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_id     TEXT NOT NULL DEFAULT '',
    kind       TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_raw_session ON raw_history(session_id, id);

CREATE TABLE IF NOT EXISTS compactions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_id     TEXT NOT NULL DEFAULT '',
    trigger_kind TEXT NOT NULL,
    summary    TEXT NOT NULL DEFAULT '',
    raw_upto   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS approvals (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    run_id       TEXT NOT NULL DEFAULT '',
    calls        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending',
    reason       TEXT NOT NULL DEFAULT '',
    req_seq      INTEGER NOT NULL DEFAULT 0,
    decision_seq INTEGER,
    created_at   INTEGER NOT NULL,
    decided_at   INTEGER
);

CREATE TABLE IF NOT EXISTS run_locks (
    session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    owner       TEXT NOT NULL,
    acquired_at INTEGER NOT NULL
);
`

// Payload size caps. The full un-truncated values live in the messages
// projection; journal payloads are audit records and stay bounded.
const (
	journalPromptMax     = 16 << 10
	journalPayloadMax    = 64 << 10
	journalToolResultMax = 16 << 10
	journalToolArgsMax   = 8 << 10
)

// lockTTL guards against locks held by processes on other hosts, or where
// liveness cannot be checked. Same-host locks are reclaimed as soon as the
// owning process is gone.
const lockTTL = 24 * time.Hour

// migrateJournal creates the journal tables and stamps the schema version.
func (s *Store) migrateJournal() error {
	if _, err := s.db.Exec(journalSchemaSQL); err != nil {
		return fmt.Errorf("journal schema: %w", err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 2`); err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

// admissionFingerprint is the canonical input identity: the same admission
// fields marshal deterministically (maps sort in encoding/json), so a
// retried request hashes identically and a changed one conflicts.
type admissionFingerprint struct {
	Model        string          `json:"model"`
	SystemPrompt string          `json:"systemPrompt,omitempty"`
	Skill        string          `json:"skill,omitempty"`
	Format       string          `json:"format,omitempty"`
	Options      map[string]any  `json:"options,omitempty"`
	Think        *api.ThinkValue `json:"think,omitempty"`
	Prompt       string          `json:"prompt"`
}

func admissionHash(ad agent.RunAdmission) string {
	fingerprint := admissionFingerprint{
		Model:        ad.Model,
		SystemPrompt: ad.SystemPrompt,
		Skill:        ad.Skill,
		Format:       ad.Format,
		Options:      ad.Options,
		Think:        ad.Think,
		Prompt:       ad.Prompt,
	}
	blob, err := json.Marshal(fingerprint)
	if err != nil {
		blob = []byte(ad.Prompt)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])
}

// AdmitRun implements agent.RunJournal. Lock acquisition, crash-residue
// recovery, request deduplication, and the run row insert all commit in one
// transaction, so two processes cannot interleave admission steps.
func (s *Store) AdmitRun(ad agent.RunAdmission) (agent.RunAdmissionResult, error) {
	if s == nil {
		return agent.RunAdmissionResult{}, fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if ad.ChatID == "" {
		return agent.RunAdmissionResult{}, fmt.Errorf("admission requires a session id")
	}

	owner := lockOwner()
	res := agent.RunAdmissionResult{}

	tx, err := s.db.Begin()
	if err != nil {
		return agent.RunAdmissionResult{}, fmt.Errorf("begin admission: %w", err)
	}
	defer tx.Rollback()

	if err := s.acquireRunLockTx(tx, ad.ChatID, owner); err != nil {
		return agent.RunAdmissionResult{}, err
	}
	// The lock is ours; any non-terminal state left behind belongs to an
	// earlier, now-gone process.
	res.Recovered = s.recoverSessionTx(tx, ad.ChatID)

	hash := admissionHash(ad)
	var prev *prevAttempt
	if ad.RequestID != "" {
		prev, err = s.latestAttemptTx(tx, ad.ChatID, ad.RequestID)
		if err != nil {
			return agent.RunAdmissionResult{}, err
		}
		if prev != nil {
			if prev.inputHash != hash {
				return agent.RunAdmissionResult{}, &agent.RunConflictError{
					RequestID:     ad.RequestID,
					ExistingRunID: prev.runID,
					Detail:        "a different canonical input was already admitted under this request id",
				}
			}
			// A committed done/denied run replays; failed, canceled, and
			// interrupted attempts may be re-admitted as a new attempt. The
			// replay runs nothing, so the lock acquired above is released in
			// the same transaction.
			if prev.status == agent.RunStatusCompleted || prev.status == "denied" {
				if _, err := tx.Exec(`DELETE FROM run_locks WHERE session_id = ?`, ad.ChatID); err != nil {
					return agent.RunAdmissionResult{}, fmt.Errorf("release run lock: %w", err)
				}
				if err := tx.Commit(); err != nil {
					return agent.RunAdmissionResult{}, fmt.Errorf("commit admission: %w", err)
				}
				if err := s.recordReplayEvent(ad.ChatID, prev.runID, ad.RequestID); err != nil {
					return agent.RunAdmissionResult{}, err
				}
				return agent.RunAdmissionResult{
					RunID:    prev.runID,
					Attempt:  prev.attempt,
					Replayed: true,
					Status:   prev.status,
				}, nil
			}
		}
	}

	attempt := 1
	if prev != nil {
		attempt = prev.attempt + 1
	}
	runID := uuid.NewString()
	res.BlockedTools = s.blockedToolsTx(tx, ad.ChatID)

	now := time.Now().Unix()
	thunk := capString(ad.Prompt, journalPromptMax)
	thinkJSON := ""
	if ad.Think != nil {
		if b, err := json.Marshal(ad.Think); err == nil {
			thinkJSON = string(b)
		}
	}
	optionsJSON := ""
	if len(ad.Options) > 0 {
		if b, err := json.Marshal(ad.Options); err == nil {
			optionsJSON = capString(string(b), journalPayloadMax)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO runs (run_id, session_id, request_id, attempt, input_hash, prompt, model, system, skill, format, options, think, status, admitted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, ad.ChatID, ad.RequestID, attempt, hash, thunk, ad.Model, ad.SystemPrompt, ad.Skill, ad.Format, optionsJSON, thinkJSON,
		agent.RunStatusAdmitted, now,
	); err != nil {
		return agent.RunAdmissionResult{}, fmt.Errorf("insert run: %w", err)
	}
	seq, err := nextEventSeq(tx, ad.ChatID)
	if err != nil {
		return agent.RunAdmissionResult{}, err
	}
	payload, _ := json.Marshal(map[string]any{"requestId": ad.RequestID, "attempt": attempt, "prompt": thunk})
	if err := insertSessionEvent(tx, eventRow{
		SessionID: ad.ChatID, Seq: seq, Kind: "run_admitted", RunID: runID,
		Payload: string(capString(string(payload), journalPayloadMax)), CreatedAt: now,
	}); err != nil {
		return agent.RunAdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return agent.RunAdmissionResult{}, fmt.Errorf("commit admission: %w", err)
	}

	res.RunID = runID
	res.Attempt = attempt
	return res, nil
}

type prevAttempt struct {
	runID     string
	attempt   int
	status    string
	inputHash string
}

func (s *Store) latestAttemptTx(tx *sql.Tx, sessionID, requestID string) (*prevAttempt, error) {
	var prev prevAttempt
	err := tx.QueryRow(
		`SELECT run_id, attempt, status, input_hash FROM runs
		 WHERE session_id = ? AND request_id = ? ORDER BY attempt DESC LIMIT 1`,
		sessionID, requestID,
	).Scan(&prev.runID, &prev.attempt, &prev.status, &prev.inputHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup request: %w", err)
	}
	return &prev, nil
}

// recordReplayEvent observes a deduplicated command in the audit trail.
func (s *Store) recordReplayEvent(sessionID, runID, requestID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin replay event: %w", err)
	}
	defer tx.Rollback()
	seq, err := nextEventSeq(tx, sessionID)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"requestId": requestID})
	if err := insertSessionEvent(tx, eventRow{
		SessionID: sessionID, Seq: seq, Kind: "run_replayed", RunID: runID,
		Payload: string(payload), CreatedAt: time.Now().Unix(),
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordToolIntent implements agent.RunJournal: the intent commits before
// the tool can be invoked.
func (s *Store) RecordToolIntent(intent agent.ToolIntent) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionID, err := s.sessionForRun(intent.RunID)
	if err != nil {
		return err
	}
	argsJSON := marshalArgs(intent.Args)
	argsHash := agent.ToolArgsKey(intent.Name, intent.Args)
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tool intent: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	if _, err := tx.Exec(
		`INSERT INTO tool_calls (tool_call_id, run_id, session_id, parent_tool_call_id, tool_name, args_hash, args, replay_safe, status, intent_seq, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		intent.ToolCallID, intent.RunID, sessionID, intent.ParentToolCallID, intent.Name, argsHash, argsJSON, boolInt(intent.ReplaySafe),
		agent.ToolJournalPending, 0, now,
	); err != nil {
		return fmt.Errorf("insert tool intent: %w", err)
	}
	seq, err := nextEventSeq(tx, sessionID)
	if err != nil {
		return err
	}
	if err := s.updateIntentSeq(tx, intent.RunID, intent.ToolCallID, seq); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"name": intent.Name, "args": argsJSON, "replaySafe": intent.ReplaySafe, "parent": intent.ParentToolCallID})
	if err := insertSessionEvent(tx, eventRow{
		SessionID: sessionID, Seq: seq, Kind: "tool_intent", RunID: intent.RunID, ToolCallID: intent.ToolCallID,
		Payload: string(capString(string(payload), journalPayloadMax)), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordToolOutcome implements agent.RunJournal.
func (s *Store) RecordToolOutcome(outcome agent.ToolOutcome) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionID, err := s.sessionForRun(outcome.RunID)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tool outcome: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	res, err := tx.Exec(
		`UPDATE tool_calls SET status = ?, result = ?, error = ?, settled_at = ?, outcome_seq = ?
		 WHERE run_id = ? AND tool_call_id = ? AND status = ?`,
		outcome.Status, capString(outcome.Result, journalToolResultMax), capString(outcome.Err, journalPayloadMax),
		now, 0, outcome.RunID, outcome.ToolCallID, agent.ToolJournalPending,
	)
	if err != nil {
		return fmt.Errorf("update tool outcome: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("tool call %s of run %s is not pending (duplicate outcome or unknown intent)", outcome.ToolCallID, outcome.RunID)
	}
	seq, err := nextEventSeq(tx, sessionID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tool_calls SET outcome_seq = ? WHERE run_id = ? AND tool_call_id = ?`, seq, outcome.RunID, outcome.ToolCallID); err != nil {
		return fmt.Errorf("set outcome seq: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"status": outcome.Status, "error": outcome.Err})
	if err := insertSessionEvent(tx, eventRow{
		SessionID: sessionID, Seq: seq, Kind: "tool_outcome", RunID: outcome.RunID, ToolCallID: outcome.ToolCallID,
		Payload: string(capString(string(payload), journalPayloadMax)), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordCompaction implements agent.RunJournal: a compaction is recorded as
// a fact covering the raw record written so far; raw rows are never
// rewritten or removed.
func (s *Store) RecordCompaction(record agent.CompactionRecord) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionID, err := s.sessionForRun(record.RunID)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin compaction: %w", err)
	}
	defer tx.Rollback()

	var rawUpto int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM raw_history WHERE session_id = ?`, sessionID).Scan(&rawUpto); err != nil {
		return fmt.Errorf("raw watermark: %w", err)
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(
		`INSERT INTO compactions (session_id, run_id, trigger_kind, summary, raw_upto, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, record.RunID, record.Trigger, record.Summary, rawUpto, now,
	); err != nil {
		return fmt.Errorf("insert compaction: %w", err)
	}
	seq, err := nextEventSeq(tx, sessionID)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"trigger": record.Trigger, "summary": record.Summary, "rawUpto": rawUpto})
	if err := insertSessionEvent(tx, eventRow{
		SessionID: sessionID, Seq: seq, Kind: "compaction", RunID: record.RunID,
		Payload: string(capString(string(payload), journalPayloadMax)), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordRawSegment implements agent.RunJournal: one span of the run's own
// messages, appended at flush time so compaction watermarks can reference it.
func (s *Store) RecordRawSegment(runID string, segment []api.Message) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(segment) == 0 {
		return nil
	}
	sessionID, err := s.sessionForRun(runID)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin raw segment: %w", err)
	}
	defer tx.Rollback()

	payload, err := json.Marshal(map[string]any{"messages": segment})
	if err != nil {
		return fmt.Errorf("marshal raw segment: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO raw_history (session_id, run_id, kind, payload, created_at) VALUES (?, ?, ?, ?, ?)`,
		sessionID, runID, "messages", string(capString(string(payload), journalPayloadMax)), time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("insert raw segment: %w", err)
	}
	return tx.Commit()
}

// RecordApprovalRequested implements agent.RunJournal.
func (s *Store) RecordApprovalRequested(record agent.ApprovalRecord) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if record.RunID == "" {
		return fmt.Errorf("approval record requires a run id")
	}
	sessionID, err := s.sessionForRun(record.RunID)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin approval: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	seq, err := nextEventSeq(tx, sessionID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO approvals (id, session_id, run_id, calls, status, req_seq, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.ApprovalID, sessionID, record.RunID, capString(record.Calls, journalPayloadMax), "pending", seq, now,
	); err != nil {
		return fmt.Errorf("insert approval: %w", err)
	}
	if err := insertSessionEvent(tx, eventRow{
		SessionID: sessionID, Seq: seq, Kind: "approval_requested", RunID: record.RunID,
		Payload: capString(record.Calls, journalPayloadMax), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// RecordApprovalDecision implements agent.RunJournal: only a pending
// approval can be decided, so a duplicate or late reply cannot authorize a
// second execution.
func (s *Store) RecordApprovalDecision(decision agent.ApprovalDecision) error {
	if s == nil {
		return fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin approval decision: %w", err)
	}
	defer tx.Rollback()

	status := "denied"
	if decision.Approved {
		status = "approved"
	}
	if decision.Expired {
		status = "expired"
	}
	var sessionID, runID string
	now := time.Now().Unix()
	res, err := tx.Exec(
		`UPDATE approvals SET status = ?, reason = ?, decision_seq = ?, decided_at = ?
		 WHERE id = ? AND status = ?`,
		status, decision.Reason, 0, now, decision.ApprovalID, "pending",
	)
	if err != nil {
		return fmt.Errorf("update approval: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var current string
		if err := tx.QueryRow(`SELECT status FROM approvals WHERE id = ?`, decision.ApprovalID).Scan(&current); err == nil {
			return fmt.Errorf("%w: approval %s is already %s", agent.ErrApprovalSettled, decision.ApprovalID, current)
		}
		return fmt.Errorf("%w: approval %s not found or already settled", agent.ErrApprovalSettled, decision.ApprovalID)
	}
	if err := tx.QueryRow(`SELECT session_id, run_id FROM approvals WHERE id = ?`, decision.ApprovalID).Scan(&sessionID, &runID); err != nil {
		return fmt.Errorf("load approval: %w", err)
	}
	seq, err := nextEventSeq(tx, sessionID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE approvals SET decision_seq = ? WHERE id = ?`, seq, decision.ApprovalID); err != nil {
		return fmt.Errorf("set decision seq: %w", err)
	}
	payload, _ := json.Marshal(map[string]any{"status": status, "reason": decision.Reason})
	if err := insertSessionEvent(tx, eventRow{
		SessionID: sessionID, Seq: seq, Kind: "approval_decided", RunID: runID,
		Payload: string(payload), CreatedAt: now,
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitRun implements agent.RunJournal: terminal run state, the message
// projection, the raw append-only record, and the closing session event
// commit in one transaction; the run lock releases with it.
func (s *Store) CommitRun(term agent.RunTerminal) (committed agent.RunCommit, err error) {
	if s == nil {
		return agent.RunCommit{}, fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if term.RunID == "" {
		return agent.RunCommit{}, fmt.Errorf("commit requires a run id")
	}
	sessionID, err := s.sessionForRun(term.RunID)
	if err != nil {
		return agent.RunCommit{}, err
	}
	// A failed terminal commit must not strand the run lock for this
	// session in this process; release best-effort on the way out.
	defer func() {
		if err != nil {
			_, _ = s.db.Exec(`DELETE FROM run_locks WHERE session_id = ? AND owner = ?`, sessionID, lockOwner())
		}
	}()

	tx, err := s.db.Begin()
	if err != nil {
		return agent.RunCommit{}, fmt.Errorf("begin commit: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	res, err := tx.Exec(
		`UPDATE runs SET status = ?, error = ?, terminal_at = ?
		 WHERE run_id = ? AND status IN (?, ?)`,
		term.Status, term.Err, now, term.RunID, agent.RunStatusAdmitted, agent.RunStatusRunning,
	)
	if err != nil {
		return agent.RunCommit{}, fmt.Errorf("commit run state: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var status string
		_ = tx.QueryRow(`SELECT status FROM runs WHERE run_id = ?`, term.RunID).Scan(&status)
		return agent.RunCommit{}, fmt.Errorf("run %s is already %s", term.RunID, status)
	}

	if len(term.History) > 0 {
		if err := syncMessagesTx(tx, sessionID, term.History); err != nil {
			return agent.RunCommit{}, err
		}
	}

	seq, err := nextEventSeq(tx, sessionID)
	if err != nil {
		return agent.RunCommit{}, err
	}
	payload, _ := json.Marshal(map[string]any{"status": term.Status, "error": term.Err})
	if err := insertSessionEvent(tx, eventRow{
		SessionID: sessionID, Seq: seq, Kind: "run_committed", RunID: term.RunID,
		Payload: string(payload), CreatedAt: now,
	}); err != nil {
		return agent.RunCommit{}, err
	}
	if _, err := tx.Exec(`UPDATE runs SET last_seq = ? WHERE run_id = ?`, seq, term.RunID); err != nil {
		return agent.RunCommit{}, fmt.Errorf("set last_seq: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM run_locks WHERE session_id = ?`, sessionID); err != nil {
		return agent.RunCommit{}, fmt.Errorf("release run lock: %w", err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return agent.RunCommit{}, fmt.Errorf("touch session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return agent.RunCommit{}, fmt.Errorf("commit run: %w", err)
	}
	return agent.RunCommit{Seq: seq}, nil
}

// syncMessagesTx aligns the stored message projection with history inside an
// open transaction: a matching prefix appends only the delta, any drift
// (compaction included) replaces the projection. Raw history is not touched.
func syncMessagesTx(tx *sql.Tx, sessionID string, history []api.Message) error {
	rows, err := tx.Query(
		`SELECT seq, role, content, thinking, tool_calls, tool_call_id, tool_name, images FROM messages WHERE session_id = ? ORDER BY seq`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("load stored messages: %w", err)
	}
	var stored []api.Message
	for rows.Next() {
		msg, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return err
		}
		stored = append(stored, msg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan stored messages: %w", err)
	}

	now := time.Now().Unix()
	if len(stored) <= len(history) && sameMessageIdentity(stored, history) {
		delta := history[len(stored):]
		for i, msg := range delta {
			if err := insertMessage(tx, sessionID, len(stored)+i, msg, now); err != nil {
				return err
			}
		}
		return touchSessionTx(tx, sessionID, delta, now)
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ?`, sessionID); err != nil {
		return fmt.Errorf("clear messages: %w", err)
	}
	for seq, msg := range history {
		if err := insertMessage(tx, sessionID, seq, msg, now); err != nil {
			return err
		}
	}
	return touchSessionTx(tx, sessionID, nil, now)
}

// touchSessionTx applies the title-from-first-user-message rule for appended
// deltas and refreshes updated_at.
func touchSessionTx(tx *sql.Tx, sessionID string, delta []api.Message, now int64) error {
	var title string
	if err := tx.QueryRow(`SELECT title FROM sessions WHERE id = ?`, sessionID).Scan(&title); err == nil && title == "" {
		for _, msg := range delta {
			if msg.Role == "user" && msg.Content != "" {
				if _, err := tx.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, deriveTitle(msg.Content), sessionID); err != nil {
					return fmt.Errorf("set title: %w", err)
				}
				break
			}
		}
	}
	if _, err := tx.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// --- run locks and recovery ---

// lockOwner identifies the owning process: host:pid. A run lock is held per
// admission and released when the run commits.
func lockOwner() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s:%d", host, os.Getpid())
}

// acquireRunLockTx claims the session's single-writer run lock inside the
// admission transaction, rejecting a live owner (second writer) and
// reclaiming a stale one (crashed owner).
func (s *Store) acquireRunLockTx(tx *sql.Tx, sessionID, owner string) error {
	var seen string
	var acquired int64
	err := tx.QueryRow(`SELECT owner, acquired_at FROM run_locks WHERE session_id = ?`, sessionID).Scan(&seen, &acquired)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// free
	case err != nil:
		return fmt.Errorf("read run lock: %w", err)
	default:
		if seen == owner {
			return &agent.RunBusyError{SessionID: sessionID, Owner: seen + " (this process)"}
		}
		if !lockStale(seen, acquired) {
			return &agent.RunBusyError{SessionID: sessionID, Owner: seen}
		}
		// Stale lock from a dead owner: reclaim with the recovery pass below.
	}
	if _, err := tx.Exec(
		`INSERT INTO run_locks (session_id, owner, acquired_at) VALUES (?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET owner = excluded.owner, acquired_at = excluded.acquired_at`,
		sessionID, owner, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("acquire run lock: %w", err)
	}
	return nil
}

func (s *Store) releaseRunLockLocked(sessionID, owner string) error {
	if _, err := s.db.Exec(`DELETE FROM run_locks WHERE session_id = ? AND owner = ?`, sessionID, owner); err != nil {
		return fmt.Errorf("release run lock: %w", err)
	}
	return nil
}

// lockStale reports whether a lock's owner is gone. Same-host locks are
// reclaimed when the PID is dead; other-host (or uncheckable) locks only
// after the TTL.
func lockStale(owner string, acquiredAt int64) bool {
	if sameHostOwner(owner) {
		if pid, ok := lockPID(owner); ok {
			if alive, known := processAlive(pid); known {
				return !alive
			}
		}
	}
	return time.Since(time.Unix(acquiredAt, 0)) > lockTTL
}

func sameHostOwner(owner string) bool {
	host, err := os.Hostname()
	if err != nil {
		return false
	}
	return len(owner) > len(host)+1 && owner[:len(host)] == host && owner[len(host)] == ':'
}

func lockPID(owner string) (int, bool) {
	for i := len(owner) - 1; i >= 0; i-- {
		if owner[i] == ':' {
			var pid int
			if _, err := fmt.Sscanf(owner[i+1:], "%d", &pid); err != nil || pid <= 0 {
				return 0, false
			}
			return pid, true
		}
	}
	return 0, false
}

// processAlive checks liveness with signal 0 where supported.
func processAlive(pid int) (alive, known bool) {
	switch runtime.GOOS {
	case "windows":
		// No signal-0 probe; liveness is unknown and only the TTL applies.
		return false, false
	default:
		p, err := os.FindProcess(pid)
		if err != nil {
			return false, true
		}
		if err := p.Signal(syscall.Signal(0)); err != nil {
			return false, true
		}
		return true, true
	}
}

// recoverSessionTx classifies state a crashed process left behind:
// un-terminated runs become interrupted, their pending tool intents become
// unknown-outcome, and their pending approvals expire (never grant). The
// run lock itself is managed by acquire/commit; classification only.
func (s *Store) recoverSessionTx(tx *sql.Tx, sessionID string) []string {
	now := time.Now().Unix()
	var lines []string

	res, err := tx.Exec(
		`UPDATE runs SET status = ?, error = ?, terminal_at = ?
		 WHERE session_id = ? AND status IN (?, ?)`,
		agent.RunStatusInterrupted, "process ended before a terminal commit (recovery)", now,
		sessionID, agent.RunStatusAdmitted, agent.RunStatusRunning,
	)
	if err != nil {
		return append(lines, "recovery query failed: "+err.Error())
	}
	if n, _ := res.RowsAffected(); n > 0 {
		lines = append(lines, fmt.Sprintf("recovered %d interrupted run(s); classification only, nothing re-executed", n))
	}

	res, err = tx.Exec(
		`UPDATE tool_calls SET status = ?, settled_at = ?
		 WHERE session_id = ? AND status = ?`,
		agent.ToolJournalUnknown, now, sessionID, agent.ToolJournalPending,
	)
	if err != nil {
		return append(lines, "tool intent recovery query failed: "+err.Error())
	}
	if n, _ := res.RowsAffected(); n > 0 {
		lines = append(lines, fmt.Sprintf("reclassified %d tool intent(s) as unknown-outcome", n))
	}

	res, err = tx.Exec(
		`UPDATE approvals SET status = ?, decided_at = ?
		 WHERE session_id = ? AND status = ? AND run_id IN
		   (SELECT run_id FROM runs WHERE session_id = ? AND status = ?)`,
		"expired", now, sessionID, "pending", sessionID, agent.RunStatusInterrupted,
	)
	if err != nil {
		return append(lines, "approval recovery query failed: "+err.Error())
	}
	if n, _ := res.RowsAffected(); n > 0 {
		lines = append(lines, fmt.Sprintf("expired %d pending approval(s) from interrupted runs", n))
	}
	return lines
}

// RecoverSession force-classifies a session's in-flight state as crash
// residue — interrupted runs, unknown-outcome tool intents, expired pending
// approvals — and clears the run lock. Only call when the session has no
// live run in any process (e.g. after confirming the owner is gone).
// AdmitRun runs the classification pass automatically after claiming the
// lock, so production paths do not need this.
func (s *Store) RecoverSession(sessionID string) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoverSessionLocked(sessionID, true)
}

// recoverSessionLocked classifies crash residue (interrupted runs,
// unknown-outcome tool intents, expired pending approvals) with plain db
// access; clears the run lock only when clearLock is set — force recovery
// for diagnostics, after confirming no live owner.
func (s *Store) recoverSessionLocked(sessionID string, clearLock bool) []string {
	now := time.Now().Unix()
	var lines []string

	res, err := s.db.Exec(
		`UPDATE runs SET status = ?, error = ?, terminal_at = ?
		 WHERE session_id = ? AND status IN (?, ?)`,
		agent.RunStatusInterrupted, "process ended before a terminal commit (recovery)", now,
		sessionID, agent.RunStatusAdmitted, agent.RunStatusRunning,
	)
	if err != nil {
		return append(lines, "recovery query failed: "+err.Error())
	}
	if n, _ := res.RowsAffected(); n > 0 {
		lines = append(lines, fmt.Sprintf("recovered %d interrupted run(s); classification only, nothing re-executed", n))
	}

	res, err = s.db.Exec(
		`UPDATE tool_calls SET status = ?, settled_at = ?
		 WHERE session_id = ? AND status = ?`,
		agent.ToolJournalUnknown, now, sessionID, agent.ToolJournalPending,
	)
	if err != nil {
		return append(lines, "tool intent recovery query failed: "+err.Error())
	}
	if n, _ := res.RowsAffected(); n > 0 {
		lines = append(lines, fmt.Sprintf("reclassified %d tool intent(s) as unknown-outcome", n))
	}

	res, err = s.db.Exec(
		`UPDATE approvals SET status = ?, decided_at = ?
		 WHERE session_id = ? AND status = ? AND run_id IN
		   (SELECT run_id FROM runs WHERE session_id = ? AND status = ?)`,
		"expired", now, sessionID, "pending", sessionID, agent.RunStatusInterrupted,
	)
	if err != nil {
		return append(lines, "approval recovery query failed: "+err.Error())
	}
	if n, _ := res.RowsAffected(); n > 0 {
		lines = append(lines, fmt.Sprintf("expired %d pending approval(s) from interrupted runs", n))
	}
	if clearLock {
		if _, err := s.db.Exec(`DELETE FROM run_locks WHERE session_id = ?`, sessionID); err != nil {
			return append(lines, "run lock cleanup failed: "+err.Error())
		}
	}
	return lines
}

// blockedToolsTx collects the (tool, args) pairs with unknown-outcome,
// non-replay-safe intents from interrupted runs, inside the admission
// transaction (the store has a single connection, so reads racing a tx must
// share it).
func (s *Store) blockedToolsTx(tx *sql.Tx, sessionID string) []agent.BlockedTool {
	rows, err := tx.Query(
		`SELECT tc.run_id, tc.tool_call_id, tc.tool_name, tc.args_hash
		 FROM tool_calls tc JOIN runs r ON r.run_id = tc.run_id
		 WHERE tc.session_id = ? AND tc.status = ? AND tc.replay_safe = 0 AND r.status = ?
		 GROUP BY tc.tool_name, tc.args_hash`,
		sessionID, agent.ToolJournalUnknown, agent.RunStatusInterrupted,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var blocked []agent.BlockedTool
	for rows.Next() {
		var b agent.BlockedTool
		if err := rows.Scan(&b.PriorRunID, &b.PriorCallID, &b.Name, &b.ArgsKey); err != nil {
			continue
		}
		b.Reason = blockedToolSummary(b)
		blocked = append(blocked, b)
	}
	return blocked
}

func blockedToolSummary(b agent.BlockedTool) string {
	return fmt.Sprintf("tool %s (call %s of run %s) has unknown outcome", b.Name, b.PriorCallID, b.PriorRunID)
}

// --- read access for tests and diagnostics ---

// RunRow is one journal run.
type RunRow struct {
	RunID     string
	SessionID string
	RequestID string
	Attempt   int
	Status    string
	Error     string
	InputHash string
}

// ToolCallRow is one journal tool call.
type ToolCallRow struct {
	ToolCallID       string
	RunID            string
	ParentToolCallID string
	Name             string
	ArgsHash         string
	Args             string
	ReplaySafe       bool
	Status           string
	Result           string
	Error            string
}

// ApprovalRow is one journal approval.
type ApprovalRow struct {
	ID       string
	RunID    string
	Status   string
	Reason   string
	Decision string
}

// EventRow is one committed session event.
type EventRow struct {
	SessionID  string
	Seq        int64
	Kind       string
	RunID      string
	ToolCallID string
	Payload    string
}

// ListRuns returns a session's journal runs in admission order.
func (s *Store) ListRuns(sessionID string) ([]RunRow, error) {
	if s == nil {
		return nil, fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT run_id, session_id, request_id, attempt, status, error, input_hash FROM runs WHERE session_id = ? ORDER BY admitted_at, rowid`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	var out []RunRow
	for rows.Next() {
		var r RunRow
		if err := rows.Scan(&r.RunID, &r.SessionID, &r.RequestID, &r.Attempt, &r.Status, &r.Error, &r.InputHash); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListToolCalls returns a session's journaled tool calls.
func (s *Store) ListToolCalls(sessionID string) ([]ToolCallRow, error) {
	if s == nil {
		return nil, fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT tool_call_id, run_id, parent_tool_call_id, tool_name, args_hash, args, replay_safe, status, result, error
		 FROM tool_calls WHERE session_id = ? ORDER BY created_at, rowid`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tool calls: %w", err)
	}
	defer rows.Close()
	var out []ToolCallRow
	for rows.Next() {
		var t ToolCallRow
		var replaySafe int
		if err := rows.Scan(&t.ToolCallID, &t.RunID, &t.ParentToolCallID, &t.Name, &t.ArgsHash, &t.Args, &replaySafe, &t.Status, &t.Result, &t.Error); err != nil {
			return nil, err
		}
		t.ReplaySafe = replaySafe == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListEvents returns a session's committed events with seq > afterSeq.
func (s *Store) ListEvents(sessionID string, afterSeq int64) ([]EventRow, error) {
	if s == nil {
		return nil, fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT session_id, seq, kind, run_id, tool_call_id, payload FROM session_events WHERE session_id = ? AND seq > ? ORDER BY seq`,
		sessionID, afterSeq,
	)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.SessionID, &e.Seq, &e.Kind, &e.RunID, &e.ToolCallID, &e.Payload); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListApprovals returns a session's journaled approvals.
func (s *Store) ListApprovals(sessionID string) ([]ApprovalRow, error) {
	if s == nil {
		return nil, fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT id, run_id, status, reason, COALESCE(status, '') FROM approvals WHERE session_id = ? ORDER BY req_seq`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list approvals: %w", err)
	}
	defer rows.Close()
	var out []ApprovalRow
	for rows.Next() {
		var a ApprovalRow
		if err := rows.Scan(&a.ID, &a.RunID, &a.Status, &a.Reason, &a.Decision); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RawHistoryCount returns the number of raw record rows for a session.
func (s *Store) RawHistoryCount(sessionID string) (int, error) {
	if s == nil {
		return 0, fmt.Errorf("nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_history WHERE session_id = ?`, sessionID).Scan(&count)
	return count, err
}

// --- small helpers ---

type eventRow struct {
	SessionID  string
	Seq        int64
	Kind       string
	Version    int
	RunID      string
	ToolCallID string
	Payload    string
	CreatedAt  int64
}

func insertSessionEvent(tx *sql.Tx, row eventRow) error {
	if row.Version == 0 {
		row.Version = 1
	}
	if row.CreatedAt == 0 {
		row.CreatedAt = time.Now().Unix()
	}
	_, err := tx.Exec(
		`INSERT INTO session_events (session_id, seq, kind, version, run_id, tool_call_id, payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.SessionID, row.Seq, row.Kind, row.Version, row.RunID, row.ToolCallID, row.Payload, row.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert event %s: %w", row.Kind, err)
	}
	return nil
}

func nextEventSeq(tx *sql.Tx, sessionID string) (int64, error) {
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM session_events WHERE session_id = ?`, sessionID).Scan(&seq); err != nil {
		return 0, fmt.Errorf("next seq: %w", err)
	}
	return seq, nil
}

func (s *Store) sessionForRun(runID string) (string, error) {
	var sessionID string
	err := s.db.QueryRow(`SELECT session_id FROM runs WHERE run_id = ?`, runID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("unknown run %s", runID)
	}
	if err != nil {
		return "", fmt.Errorf("lookup run: %w", err)
	}
	return sessionID, nil
}

func (s *Store) updateIntentSeq(tx *sql.Tx, runID, toolCallID string, seq int64) error {
	if _, err := tx.Exec(`UPDATE tool_calls SET intent_seq = ? WHERE run_id = ? AND tool_call_id = ?`, seq, runID, toolCallID); err != nil {
		return fmt.Errorf("set intent seq: %w", err)
	}
	return nil
}

func marshalArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return capString(string(b), journalToolArgsMax)
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// capString truncates to max bytes on a rune boundary. Capped payload fields
// are audit text, not re-parsed JSON.
func capString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut, size := 0, 0
	for i, r := range s {
		size += utf8.RuneLen(r)
		if size > max {
			break
		}
		cut = i + utf8.RuneLen(r)
	}
	return s[:cut]
}
