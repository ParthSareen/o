package mcpclient

// OAuth token/registration cache. Safe-local guarantees:
//   - file is written 0600 inside ~/.ollama (dir 0700)
//   - tokens are never included in errors or warnings
//   - corrupt/unreadable cache degrades to "not logged in", never a crash

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type oauthEntry struct {
	ServerURL     string    `json:"server_url"`
	Issuer        string    `json:"issuer,omitempty"`
	TokenEndpoint string    `json:"token_endpoint,omitempty"`
	ClientID      string    `json:"client_id,omitempty"`
	ClientSecret  string    `json:"client_secret,omitempty"` // only if the AS issued one to a DCR'd client
	RedirectURI   string    `json:"redirect_uri,omitempty"`
	AccessToken   string    `json:"access_token,omitempty"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	TokenType     string    `json:"token_type,omitempty"`
	Expiry        time.Time `json:"expiry,omitempty"`
}

func (e *oauthEntry) hasToken() bool  { return e != nil && e.AccessToken != "" }
func (e *oauthEntry) hasClient() bool { return e != nil && e.ClientID != "" }
func (e *oauthEntry) redirectPort() string {
	if e == nil || e.RedirectURI == "" {
		return ""
	}
	// http://127.0.0.1:<port>/callback
	if i := strings.LastIndexByte(e.RedirectURI, ':'); i >= 0 {
		p := e.RedirectURI[i+1:]
		if j := strings.IndexByte(p, '/'); j >= 0 {
			return p[:j]
		}
		return p
	}
	return ""
}

type oauthCache struct {
	mu      sync.Mutex
	path    string
	entries map[string]*oauthEntry
	loaded  bool
}

func newOAuthCache(path string) *oauthCache {
	return &oauthCache{path: path, entries: map[string]*oauthEntry{}}
}

func defaultOAuthCache() *oauthCache {
	home, err := os.UserHomeDir()
	if err != nil {
		return newOAuthCache("")
	}
	return newOAuthCache(filepath.Join(home, ".ollama", "mcp-oauth.json"))
}

func (c *oauthCache) load() {
	if c.loaded || c.path == "" {
		return
	}
	c.loaded = true
	data, err := os.ReadFile(c.path)
	if err != nil {
		return // missing or unreadable: start empty
	}
	var raw struct {
		Entries map[string]*oauthEntry `json:"entries"`
	}
	if json.Unmarshal(data, &raw) == nil && raw.Entries != nil {
		c.entries = raw.Entries
	}
}

func (c *oauthCache) get(server string) *oauthEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	e := c.entries[server]
	if e == nil {
		return nil
	}
	cp := *e
	return &cp
}

func (c *oauthCache) put(server string, e *oauthEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	cp := *e
	c.entries[server] = &cp
	c.saveLocked()
}

// update merges fields of e into the stored entry without clobbering fields
// the caller didn't set (empty strings mean "keep").
func (c *oauthCache) update(server string, patch func(*oauthEntry)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	e := c.entries[server]
	if e == nil {
		e = &oauthEntry{}
		c.entries[server] = e
	}
	patch(e)
	c.saveLocked()
}

func (c *oauthCache) clearTokens(server string) {
	c.update(server, func(e *oauthEntry) {
		e.AccessToken, e.RefreshToken, e.TokenType = "", "", ""
		e.Expiry = time.Time{}
	})
}

func (c *oauthCache) saveLocked() {
	if c.path == "" {
		return
	}
	data, err := json.Marshal(map[string]any{"entries": c.entries})
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.path), 0o700)
	// write-then-chmod so an existing wider-permission file is also fixed
	if err := os.WriteFile(c.path, data, 0o600); err == nil {
		_ = os.Chmod(c.path, 0o600)
	}
}
