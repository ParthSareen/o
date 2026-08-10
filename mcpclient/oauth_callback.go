package mcpclient

// Loopback OAuth callback listener. Safe-local guarantees:
//   - binds 127.0.0.1 only, never a routable interface
//   - ephemeral port (optionally re-binding a previously-registered port)
//   - cryptographically random `state`, compared in constant time
//   - one-shot: closes after the first /callback request or ctx timeout
//   - serves a fixed success page; request data is only forwarded to the
//     in-process channel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"sync"
)

type callbackResult struct {
	code  string
	state string
	iss   string
	err   error
}

type callbackListener struct {
	ln   net.Listener
	srv  *http.Server
	ch   chan callbackResult
	once sync.Once

	mu    sync.Mutex
	state string // expected state, set when the consent URL is known
}

func newState() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// startCallbackListener binds a loopback listener, preferring preferredPort
// (so a previously-registered redirect URI stays valid), falling back to an
// ephemeral port. The expected state is set later via expectState once the
// authorization URL (which carries it) is known.
func startCallbackListener(preferredPort string) (*callbackListener, error) {
	var ln net.Listener
	var err error
	if preferredPort != "" {
		ln, err = net.Listen("tcp", "127.0.0.1:"+preferredPort)
	}
	if ln == nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, fmt.Errorf("bind loopback callback: %w", err)
	}

	l := &callbackListener{ln: ln, ch: make(chan callbackResult, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", l.handle)
	l.srv = &http.Server{Handler: mux}
	go func() { _ = l.srv.Serve(ln) }()
	return l, nil
}

// expectState records the state value the next /callback request must carry.
func (l *callbackListener) expectState(state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = state
}

func (l *callbackListener) expectedState() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state
}

func (l *callbackListener) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	gotState := q.Get("state")
	want := l.expectedState()
	// constant-time comparison; length check keeps ConstantTimeCompare sane
	if want == "" || len(gotState) != len(want) || subtle.ConstantTimeCompare([]byte(gotState), []byte(want)) != 1 {
		l.deliver(callbackResult{err: fmt.Errorf("state mismatch (possible CSRF); authorization aborted")})
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		l.deliver(callbackResult{err: fmt.Errorf("authorization failed: %s (%s)", e, desc)})
		fmt.Fprint(w, callbackPage("Authorization failed, you can close this tab."))
		return
	}
	code := q.Get("code")
	if code == "" {
		l.deliver(callbackResult{err: fmt.Errorf("no authorization code in callback")})
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	l.deliver(callbackResult{code: code, state: gotState, iss: q.Get("iss")})
	fmt.Fprint(w, callbackPage("Authorization complete — you can close this tab and return to the terminal."))
}

func (l *callbackListener) deliver(res callbackResult) {
	l.once.Do(func() { l.ch <- res })
}

func (l *callbackListener) url() string {
	return fmt.Sprintf("http://%s/callback", l.ln.Addr().String())
}

// wait blocks until the callback arrives or ctx (timeout/cancel) fires.
func (l *callbackListener) wait(ctx context.Context) (callbackResult, error) {
	select {
	case res := <-l.ch:
		return res, res.err
	case <-ctx.Done():
		return callbackResult{}, fmt.Errorf("timed out waiting for authorization callback: %w", ctx.Err())
	}
}

func (l *callbackListener) close() {
	_ = l.srv.Close()
}

func callbackPage(msg string) string {
	return "<!doctype html><meta charset=utf-8><title>o mcp auth</title>" +
		"<body style='font-family:system-ui;max-width:32em;margin:4em auto'>" +
		"<h2>o — MCP authorization</h2><p>" + msg + "</p></body>"
}
