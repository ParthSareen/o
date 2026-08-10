package mcpclient

// OAuth tests: the full consent flow is exercised against a fake OAuth
// authorization server (httptest, loopback http) through the real SDK
// authorization-code handler and the real loopback callback listener. The
// "browser" is a fake opener that follows the authorize redirect
// programmatically.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// --- cache ---

func TestOAuthCacheRoundTripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "mcp-oauth.json")
	c := newOAuthCache(path)
	c.put("srv", &oauthEntry{
		ServerURL: "https://x", Issuer: "https://x", TokenEndpoint: "https://x/token",
		ClientID: "cid", AccessToken: "at", RefreshToken: "rt", TokenType: "Bearer",
		Expiry: time.Now().Add(time.Hour).Truncate(time.Second),
	})

	c2 := newOAuthCache(path)
	e := c2.get("srv")
	if e == nil || e.AccessToken != "at" || e.RefreshToken != "rt" || e.ClientID != "cid" {
		t.Fatalf("entry = %+v", e)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache perms = %o, want 600", fi.Mode().Perm())
	}
}

func TestOAuthCacheCorruptTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp-oauth.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newOAuthCache(path)
	if e := c.get("srv"); e != nil {
		t.Fatalf("entry = %+v, want nil", e)
	}
	c.put("srv", &oauthEntry{AccessToken: "at"}) // must not panic; overwrites
	if got := newOAuthCache(path).get("srv").AccessToken; got != "at" {
		t.Fatalf("got %q", got)
	}
}

func TestOAuthCacheClearTokensKeepsRegistration(t *testing.T) {
	c := newOAuthCache(filepath.Join(t.TempDir(), "c.json"))
	c.put("s", &oauthEntry{ClientID: "cid", AccessToken: "at", RefreshToken: "rt", TokenEndpoint: "https://x/token"})
	c.clearTokens("s")
	e := c.get("s")
	if e.AccessToken != "" || e.RefreshToken != "" {
		t.Fatalf("tokens not cleared: %+v", e)
	}
	if e.ClientID != "cid" || e.TokenEndpoint == "" {
		t.Fatalf("registration lost: %+v", e)
	}
}

func TestRedirectPortParsing(t *testing.T) {
	e := &oauthEntry{RedirectURI: "http://127.0.0.1:54321/callback"}
	if p := e.redirectPort(); p != "54321" {
		t.Fatalf("port = %q", p)
	}
	if p := (&oauthEntry{}).redirectPort(); p != "" {
		t.Fatalf("port = %q", p)
	}
}

// --- callback listener ---

func TestCallbackListenerLoopbackOnly(t *testing.T) {
	l, err := startCallbackListener("")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	host := l.ln.Addr().String()
	if !strings.HasPrefix(host, "127.0.0.1:") {
		t.Fatalf("listener bound to %q, want 127.0.0.1", host)
	}
}

func TestCallbackStateMismatchRejected(t *testing.T) {
	state := newState()
	l, err := startCallbackListener("")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	l.expectState(state)
	url := l.url() + "?code=abc&state=WRONG"
	resp, err := http.Get(url) //nolint:bodyclose
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	res, err := l.wait(context.Background())
	if err == nil || !strings.Contains(res.err.Error(), "state mismatch") {
		t.Fatalf("res = %+v err = %v", res, err)
	}
}

func TestCallbackValidStateCaptured(t *testing.T) {
	state := newState()
	l, err := startCallbackListener("")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	l.expectState(state)
	resp, err := http.Get(l.url() + "?code=THECODE&state=" + state + "&iss=https://issuer")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	res, err := l.wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.code != "THECODE" || res.state != state || res.iss != "https://issuer" {
		t.Fatalf("res = %+v", res)
	}
}

func TestCallbackTimeout(t *testing.T) {
	l, err := startCallbackListener("")
	if err != nil {
		t.Fatal(err)
	}
	defer l.close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.wait(ctx); err == nil {
		t.Fatal("want timeout error")
	}
}

// --- fake OAuth authorization server ---

type fakeAS struct {
	t *testing.T

	// issAdvertised: emit authorization_response_iss_parameter_supported in metadata
	issAdvertised bool
	// issValue: iss value the /authorize redirect carries ("" = none)
	issValue string

	mu           sync.Mutex
	clientIDs    int
	accessTokens int
	gotTokenAuth string

	base string // filled after httptest starts
}

func (f *fakeAS) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"resource":              f.base + "/mcp",
			"authorization_servers": []string{f.base},
			"scopes_supported":      []string{"read"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		meta := map[string]any{
			"issuer":                           f.base,
			"authorization_endpoint":           f.base + "/authorize",
			"token_endpoint":                   f.base + "/token",
			"registration_endpoint":            f.base + "/register",
			"scopes_supported":                 []string{"read", "offline_access"},
			"code_challenge_methods_supported": []string{"S256"},
		}
		if f.issAdvertised {
			meta["authorization_response_iss_parameter_supported"] = true
		}
		json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		f.clientIDs++
		f.mu.Unlock()
		var meta map[string]any
		_ = json.NewDecoder(r.Body).Decode(&meta)
		json.NewEncoder(w).Encode(map[string]any{"client_id": fmt.Sprintf("client-%d", f.clientIDs)})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("redirect_uri") == "" || q.Get("state") == "" {
			http.Error(w, "bad request", 400)
			return
		}
		// approve instantly: redirect back with a code
		u, _ := url.Parse(q.Get("redirect_uri"))
		qq := u.Query()
		qq.Set("code", "AUTHCODE")
		qq.Set("state", q.Get("state"))
		if f.issValue != "" {
			qq.Set("iss", f.issValue)
		}
		u.RawQuery = qq.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		grant := r.Form.Get("grant_type")
		if grant == "refresh_token" && r.Form.Get("refresh_token") == "REVOKED" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		f.mu.Lock()
		f.accessTokens++
		n := f.accessTokens
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  fmt.Sprintf("ACCESS-%d", n),
			"refresh_token": "REFRESH-1",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	return mux
}

// fakeBrowser returns an openBrowserFunc that simulates the user approving in
// a browser: fetch the authorize URL without following redirects, then fetch
// the redirect (the loopback callback).
func fakeBrowser(t *testing.T) openBrowserFunc {
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return func(authURL string) {
		resp, err := noRedirect.Get(authURL)
		if err != nil {
			t.Errorf("browser: authorize: %v", err)
			return
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			t.Errorf("browser: no redirect from authorize")
			return
		}
		resp2, err := http.Get(loc) // hits the loopback callback
		if err != nil {
			t.Errorf("browser: callback: %v", err)
			return
		}
		resp2.Body.Close()
	}
}

func testHandler(t *testing.T, cache *oauthCache, serverURL string, sc ServerConfig, open openBrowserFunc) *oauthHandler {
	t.Helper()
	if open == nil {
		open = fakeBrowser(t)
	}
	return newOAuthHandler("test-srv", serverURL, sc, cache, open, io.Discard, http.DefaultClient)
}

func fake401(requestURL, resourceMetaURL string) (*http.Request, *http.Response) {
	req, _ := http.NewRequest("POST", requestURL, nil)
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{},
		Body:       http.NoBody,
	}
	resp.Header.Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s"`, resourceMetaURL))
	return req, resp
}

func TestOAuthFullConsentFlow(t *testing.T) {
	fake := &fakeAS{t: t, issAdvertised: true}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()
	fake.base = srv.URL
	fake.issValue = srv.URL

	cache := newOAuthCache(filepath.Join(t.TempDir(), "mcp-oauth.json"))
	serverURL := srv.URL + "/mcp"
	h := testHandler(t, cache, serverURL, ServerConfig{URL: serverURL}, nil)

	// pre-auth: no token
	if ts, _ := h.TokenSource(context.Background()); ts != nil {
		t.Fatal("TokenSource before auth should be nil")
	}

	req, resp := fake401(srv.URL+"/mcp", srv.URL+"/.well-known/oauth-protected-resource")
	if err := h.Authorize(context.Background(), req, resp); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	ts, err := h.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatal("no token source after consent")
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok.AccessToken, "ACCESS-") {
		t.Fatalf("access token = %q", tok.AccessToken)
	}

	// persisted: tokens AND registration, so a restarted process skips consent
	e := cache.get("test-srv")
	if e.AccessToken == "" || e.RefreshToken == "" || e.ClientID != "client-1" || e.TokenEndpoint == "" || e.RedirectURI == "" {
		t.Fatalf("cached entry = %+v", e)
	}

	// a brand-new handler on the same cache must not re-run discovery/DCR
	fake.mu.Lock()
	registrations := fake.clientIDs
	fake.mu.Unlock()
	h2 := testHandler(t, cache, serverURL, ServerConfig{URL: serverURL}, nil)
	ts2, _ := h2.TokenSource(context.Background())
	if ts2 == nil {
		t.Fatal("cache not reused across handler instances")
	}
	if got, _ := ts2.Token(); !strings.HasPrefix(got.AccessToken, "ACCESS-") {
		t.Fatalf("token = %q", got.AccessToken)
	}
	fake.mu.Lock()
	if fake.clientIDs != registrations {
		t.Fatalf("unexpected extra registration: %d -> %d", registrations, fake.clientIDs)
	}
	fake.mu.Unlock()
}

func TestOAuthRefreshOnRestart(t *testing.T) {
	fake := &fakeAS{t: t}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()
	fake.base = srv.URL

	cache := newOAuthCache(filepath.Join(t.TempDir(), "mcp-oauth.json"))
	// seed: expired access + refresh, valid registration
	cache.put("test-srv", &oauthEntry{
		ServerURL: srv.URL + "/mcp", Issuer: srv.URL, TokenEndpoint: srv.URL + "/token",
		ClientID: "client-1", RefreshToken: "REFRESH-1", AccessToken: "ACCESS-OLD",
		TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour),
	})

	h := testHandler(t, cache, srv.URL+"/mcp", ServerConfig{}, nil)
	ts, err := h.TokenSource(context.Background())
	if err != nil || ts == nil {
		t.Fatal("want refreshed token source")
	}
	tok, _ := ts.Token()
	if tok.AccessToken != "ACCESS-1" {
		t.Fatalf("access = %q, want freshly minted ACCESS-1", tok.AccessToken)
	}
	if e := cache.get("test-srv"); e.AccessToken != "ACCESS-1" || e.RefreshToken != "REFRESH-1" {
		t.Fatalf("cache after refresh = %+v", e)
	}
}

func TestOAuthRevokedRefreshClearsTokens(t *testing.T) {
	fake := &fakeAS{t: t}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()
	fake.base = srv.URL

	cache := newOAuthCache(filepath.Join(t.TempDir(), "mcp-oauth.json"))
	cache.put("test-srv", &oauthEntry{
		Issuer: srv.URL, TokenEndpoint: srv.URL + "/token", ClientID: "client-1",
		AccessToken: "OLD", RefreshToken: "REVOKED", TokenType: "Bearer",
		Expiry: time.Now().Add(-time.Hour),
	})

	h := testHandler(t, cache, srv.URL+"/mcp", ServerConfig{}, nil)
	ts, _ := h.TokenSource(context.Background())
	if ts != nil {
		t.Fatal("revoked refresh must yield nil token source (401 path)")
	}
	if e := cache.get("test-srv"); e.AccessToken != "" || e.RefreshToken != "" {
		t.Fatalf("tokens not cleared: %+v", e)
	}
}

func TestOAuthConsentDeniedPropagates(t *testing.T) {
	fake := &fakeAS{t: t}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()
	fake.base = srv.URL

	// browser that hits the callback with an error instead of a code
	denyBrowser := func(authURL string) {
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(authURL)
		if err != nil {
			return
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		u, _ := url.Parse(loc)
		q := u.Query()
		q.Del("code")
		q.Set("error", "access_denied")
		u.RawQuery = q.Encode()
		r2, err := http.Get(u.String())
		if err == nil {
			r2.Body.Close()
		}
	}

	h := testHandler(t, newOAuthCache(filepath.Join(t.TempDir(), "c.json")), srv.URL+"/mcp", ServerConfig{URL: srv.URL + "/mcp"}, denyBrowser)
	req, resp := fake401(srv.URL+"/mcp", srv.URL+"/.well-known/oauth-protected-resource")
	if err := h.Authorize(context.Background(), req, resp); err == nil {
		t.Fatal("want consent-denied error")
	} else if strings.Contains(strings.ToLower(err.Error()), "token") && strings.Contains(err.Error(), "ACCESS") {
		t.Fatalf("error leaks token material: %v", err)
	}
}

var _ = tls.Config{} // silence unused import when TLS tests are added

var _ = oauth2.Token{} // package under use: oauth2 tokens flow through

func TestOAuthIssReturnedButNotAdvertised(t *testing.T) {
	// Mintlify behavior: iss present in the redirect, not advertised in metadata.
	// Must succeed, with iss verified internally.
	fake := &fakeAS{t: t, issAdvertised: false}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()
	fake.base = srv.URL
	fake.issValue = srv.URL

	cache := newOAuthCache(filepath.Join(t.TempDir(), "mcp-oauth.json"))
	serverURL := srv.URL + "/mcp"
	h := testHandler(t, cache, serverURL, ServerConfig{URL: serverURL}, nil)

	req, resp := fake401(srv.URL+"/mcp", srv.URL+"/.well-known/oauth-protected-resource")
	if err := h.Authorize(context.Background(), req, resp); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	ts, _ := h.TokenSource(context.Background())
	if ts == nil {
		t.Fatal("no token source after consent")
	}
	if tok, _ := ts.Token(); !strings.HasPrefix(tok.AccessToken, "ACCESS-") {
		t.Fatalf("access = %q", tok.AccessToken)
	}
}

func TestOAuthIssuerMismatchNotAdvertised(t *testing.T) {
	// mix-up protection must still fire when the server doesn't advertise iss support
	fake := &fakeAS{t: t, issAdvertised: false, issValue: "https://evil.example.com"}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()
	fake.base = srv.URL

	h := testHandler(t, newOAuthCache(filepath.Join(t.TempDir(), "c.json")), srv.URL+"/mcp", ServerConfig{URL: srv.URL + "/mcp"}, nil)
	req, resp := fake401(srv.URL+"/mcp", srv.URL+"/.well-known/oauth-protected-resource")
	err := h.Authorize(context.Background(), req, resp)
	if err == nil || !strings.Contains(err.Error(), "mix-up") {
		t.Fatalf("err = %v, want mix-up rejection", err)
	}
}

func TestOAuthIssuerMismatchAdvertised(t *testing.T) {
	// with advertised support, the SDK performs the strict check
	fake := &fakeAS{t: t, issAdvertised: true, issValue: "https://evil.example.com"}
	srv := httptest.NewServer(fake.mux())
	defer srv.Close()
	fake.base = srv.URL

	h := testHandler(t, newOAuthCache(filepath.Join(t.TempDir(), "c.json")), srv.URL+"/mcp", ServerConfig{URL: srv.URL + "/mcp"}, nil)
	req, resp := fake401(srv.URL+"/mcp", srv.URL+"/.well-known/oauth-protected-resource")
	if err := h.Authorize(context.Background(), req, resp); err == nil {
		t.Fatal("want issuer-mismatch rejection from SDK")
	}
}
