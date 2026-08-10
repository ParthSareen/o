package mcpclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/ParthSareen/o/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectTimeout bounds each server's connect+initialize handshake.
const connectTimeout = 20 * time.Second

// ServerTool pairs one MCP tool with the session it came from.
type ServerTool struct {
	Server  string
	Tool    *mcp.Tool
	Session *mcp.ClientSession
}

// Manager owns MCP client sessions for the process lifetime.
type Manager struct {
	mu       sync.Mutex
	client   *mcp.Client
	sessions []*mcp.ClientSession
	tools    []ServerTool
	warnings []string

	// OAuth wiring for URL servers
	oauthCache  *oauthCache
	openBrowser openBrowserFunc
	consentOut  io.Writer
}

// ManagerOption customizes NewManager.
type ManagerOption func(*Manager)

// WithOAuth configures how URL servers perform OAuth consent: open opens the
// authorization URL in a browser (nil = print only), consentOut receives the
// consent prompts. The token cache defaults to ~/.ollama/mcp-oauth.json.
func WithOAuth(open openBrowserFunc, consentOut io.Writer) ManagerOption {
	return func(m *Manager) {
		m.openBrowser = open
		m.consentOut = consentOut
	}
}

func withOAuthCache(c *oauthCache) ManagerOption {
	return func(m *Manager) { m.oauthCache = c }
}

// NewManager creates a manager. clientInfo identifies this client to servers.
func NewManager(clientInfo *mcp.Implementation, opts ...ManagerOption) *Manager {
	m := &Manager{
		client:     mcp.NewClient(clientInfo, nil),
		oauthCache: defaultOAuthCache(),
		consentOut: io.Discard,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Connect connects every configured server and snapshots their tools. Servers
// that fail are recorded as warnings and skipped so one bad server never
// blocks the harness.
func (m *Manager) Connect(ctx context.Context, cfg *Config) {
	if cfg == nil {
		return
	}
	for name, sc := range cfg.Servers {
		if err := m.connectServer(ctx, name, sc); err != nil {
			m.warn(fmt.Sprintf("mcp server %q: %v", name, err))
		}
	}
}

func (m *Manager) connectServer(ctx context.Context, name string, sc ServerConfig) error {
	transport, err := m.transportFor(name, sc)
	if err != nil {
		return err
	}
	connectCtx := ctx
	var cancel context.CancelFunc
	// OAuth-capable URL servers may block on interactive browser consent;
	// their budget is consentTimeout, not the handshake timeout.
	if sc.URL == "" || (sc.OAuth != nil && sc.OAuth.Disable) {
		connectCtx, cancel = context.WithTimeout(ctx, connectTimeout)
		defer cancel()
	}
	session, err := m.client.Connect(connectCtx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	m.mu.Lock()
	m.sessions = append(m.sessions, session)
	m.mu.Unlock()

	var cursor string
	for {
		res, err := session.ListTools(connectCtx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			m.warn(fmt.Sprintf("mcp server %q: list tools: %v", name, err))
			return nil
		}
		m.mu.Lock()
		for _, t := range res.Tools {
			m.tools = append(m.tools, ServerTool{Server: name, Tool: t, Session: session})
		}
		m.mu.Unlock()
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return nil
}

func (m *Manager) transportFor(serverName string, sc ServerConfig) (mcp.Transport, error) {
	if sc.URL != "" {
		t := &mcp.StreamableClientTransport{Endpoint: sc.URL}
		httpClient := http.DefaultClient
		if len(sc.Headers) > 0 {
			httpClient = &http.Client{Transport: headerRoundTripper{headers: sc.Headers, base: http.DefaultTransport}}
		}
		oauthDisabled := sc.OAuth != nil && sc.OAuth.Disable
		t.HTTPClient = httpClient
		if !oauthDisabled {
			t.OAuthHandler = newOAuthHandler(serverName, sc.URL, sc, m.oauthCache, m.openBrowser, m.consentOut, httpClient)
		}
		return t, nil
	}
	cmd := exec.Command(sc.Command, sc.Args...)
	if len(sc.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range sc.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	return &mcp.CommandTransport{Command: cmd}, nil
}

type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// RegisterAll registers every connected MCP tool into the agent registry as
// mcp__<server>__<tool> and returns the count registered. A tool whose
// computed name is already taken is skipped with a warning.
func (m *Manager) RegisterAll(r *agent.Registry) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, st := range m.tools {
		a := newAdapter(st)
		if _, exists := r.Get(a.Name()); exists {
			m.warnings = append(m.warnings, fmt.Sprintf("mcp tool %s skipped: name already registered", a.Name()))
			continue
		}
		r.Register(a)
		n++
	}
	return n
}

// Tools returns the snapshot of discovered tools.
func (m *Manager) Tools() []ServerTool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ServerTool(nil), m.tools...)
}

// Warnings returns accumulated non-fatal problems (failed servers, name
// collisions).
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.warnings...)
}

func (m *Manager) warn(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warnings = append(m.warnings, msg)
}

// Close shuts down all sessions (terminating stdio child processes).
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for _, s := range m.sessions {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.sessions = nil
	return firstErr
}
