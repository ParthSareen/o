package mcpclient

// OAuthHandler implementation for URL (streamable HTTP) MCP servers.
//
// Design: the SDK's auth.AuthorizationCodeHandler performs the OAuth flow
// (discovery, PKCE, state + issuer validation, code exchange), but keeps
// tokens in memory only. This wrapper adds what a CLI needs around it:
//
//   - persistent token + DCR registration cache (oauth_cache.go)
//   - safe loopback callback listener (oauth_callback.go)
//   - token refresh on process restart via the cached token endpoint
//   - consent UX: prints the URL to stderr and opens the browser
//
// Consent needs a human at a browser exactly once per server; afterwards the
// cached token (and refresh token) is used silently.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// consentTimeout bounds the browser-consent round trip.
const consentTimeout = 5 * time.Minute

// openBrowserFunc is injectable for tests. The zero value uses the harness
// browser opener.
type openBrowserFunc func(url string)

type oauthHandler struct {
	serverName string
	serverURL  string
	cache      *oauthCache
	open       openBrowserFunc
	consentOut io.Writer // stderr
	httpClient *http.Client

	// preregistered overrides from config
	cfgClientID     string
	cfgClientSecret string

	mu    sync.Mutex
	token *oauth2.Token // current in-process token
}

func newOAuthHandler(serverName, serverURL string, sc ServerConfig, cache *oauthCache, open openBrowserFunc, consentOut io.Writer, httpClient *http.Client) *oauthHandler {
	h := &oauthHandler{
		serverName: serverName,
		serverURL:  serverURL,
		cache:      cache,
		open:       open,
		consentOut: consentOut,
		httpClient: httpClient,
	}
	if sc.OAuth != nil {
		h.cfgClientID = sc.OAuth.ClientID
		h.cfgClientSecret = sc.OAuth.ClientSecret
	}
	return h
}

// TokenSource is called by the transport before each request. It returns a
// source for a cached/refreshed token when one is available, else nil (the
// request then goes out unauthenticated and a 401 triggers Authorize).
func (h *oauthHandler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if tok := h.currentTokenLocked(); tok != nil {
		if tok.Valid() {
			return oauth2.StaticTokenSource(tok), nil
		}
		if refreshed := h.refreshLocked(ctx, tok); refreshed != nil {
			return oauth2.StaticTokenSource(refreshed), nil
		}
		// stale and unrefreshable: fall through to nil so the 401 path runs
	}
	return nil, nil
}

func (h *oauthHandler) currentTokenLocked() *oauth2.Token {
	if h.token != nil && h.token.AccessToken != "" {
		return h.token
	}
	e := h.cache.get(h.serverName)
	if !e.hasToken() {
		return nil
	}
	return &oauth2.Token{
		AccessToken:  e.AccessToken,
		RefreshToken: e.RefreshToken,
		TokenType:    firstNonEmpty(e.TokenType, "Bearer"),
		Expiry:       e.Expiry,
	}
}

// refreshLocked exchanges a refresh token using the cached registration.
func (h *oauthHandler) refreshLocked(ctx context.Context, expired *oauth2.Token) *oauth2.Token {
	if expired.RefreshToken == "" {
		return nil
	}
	e := h.cache.get(h.serverName)
	if !e.hasClient() || e.TokenEndpoint == "" {
		return nil
	}
	cfg := &oauth2.Config{
		ClientID:     e.ClientID,
		ClientSecret: e.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: e.TokenEndpoint},
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tok, err := cfg.TokenSource(rctx, expired).Token()
	if err != nil || tok.AccessToken == "" {
		// revoked/expired refresh token etc.: drop tokens, keep registration
		h.cache.clearTokens(h.serverName)
		return nil
	}
	h.token = tok
	h.cache.update(h.serverName, func(en *oauthEntry) {
		en.AccessToken = tok.AccessToken
		if tok.RefreshToken != "" {
			en.RefreshToken = tok.RefreshToken
		}
		en.TokenType = tok.TokenType
		en.Expiry = tok.Expiry
	})
	return tok
}

// Authorize performs the full consent flow for one server: metadata
// discovery, client registration (cached or dynamic), loopback callback,
// browser consent (via the SDK's authorization-code handler), then persists
// the resulting tokens.
func (h *oauthHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	wwwChallenges, _ := oauthex.ParseWWWAuthenticate(resp.Header[http.CanonicalHeaderKey("WWW-Authenticate")])
	metadataURL := ""
	for _, c := range wwwChallenges {
		if u := c.Params["resource_metadata"]; u != "" {
			metadataURL = u
			break
		}
	}
	if metadataURL == "" {
		metadataURL = wellKnownResourceMetadataURL(h.serverURL)
	}

	prm, err := oauthex.GetProtectedResourceMetadata(ctx, metadataURL, h.serverURL, h.httpClient)
	if err != nil {
		return fmt.Errorf("oauth discovery failed for %s: %w", h.serverURL, err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return fmt.Errorf("oauth discovery for %s returned no authorization servers", h.serverURL)
	}

	asm, err := auth.GetAuthServerMetadata(ctx, prm.AuthorizationServers[0], h.httpClient)
	if err != nil || asm == nil {
		// spec fallback: derive endpoints from the authorization server base
		as := prm.AuthorizationServers[0]
		asm = &oauthex.AuthServerMeta{
			Issuer:                as,
			AuthorizationEndpoint: as + "/authorize",
			TokenEndpoint:         as + "/token",
			RegistrationEndpoint:  as + "/register",
		}
	}

	// listener first: we need the redirect URI for registration
	cached := h.cache.get(h.serverName)
	listener, err := startCallbackListener(cached.redirectPort())
	if err != nil {
		return err
	}
	defer listener.close()
	redirectURI := listener.url()

	clientID, clientSecret, err := h.ensureClient(ctx, asm, cached, redirectURI)
	if err != nil {
		return err
	}

	inner, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      clientCredentials(clientID, clientSecret, asm.Issuer),
		RedirectURL:              redirectURI,
		AuthorizationCodeFetcher: h.consentFetcher(listener, asm),
		RequestRefreshToken:      true,
		Client:                   h.httpClient,
	})
	if err != nil {
		return err
	}

	if err := inner.Authorize(ctx, req, resp); err != nil {
		return fmt.Errorf("oauth authorization failed: %w", err)
	}

	ts, err := inner.TokenSource(ctx)
	if err != nil || ts == nil {
		return fmt.Errorf("oauth flow completed but no token source was established")
	}
	tok, err := ts.Token()
	if err != nil {
		return fmt.Errorf("oauth token exchange failed: %w", err)
	}

	h.mu.Lock()
	h.token = tok
	h.mu.Unlock()
	h.cache.update(h.serverName, func(e *oauthEntry) {
		e.ServerURL = h.serverURL
		e.Issuer = asm.Issuer
		e.TokenEndpoint = asm.TokenEndpoint
		e.ClientID = clientID
		e.ClientSecret = clientSecret
		e.RedirectURI = redirectURI
		e.AccessToken = tok.AccessToken
		if tok.RefreshToken != "" {
			e.RefreshToken = tok.RefreshToken
		}
		e.TokenType = tok.TokenType
		e.Expiry = tok.Expiry
	})
	fmt.Fprintf(h.consentOut, "mcp server %q: authorization saved (cached under ~/.ollama)\n", h.serverName)
	return nil
}

// ensureClient resolves a usable (clientID, secret): config override first,
// then the cache, then dynamic client registration.
func (h *oauthHandler) ensureClient(ctx context.Context, asm *oauthex.AuthServerMeta, cached *oauthEntry, redirectURI string) (string, string, error) {
	if h.cfgClientID != "" {
		return h.cfgClientID, h.cfgClientSecret, nil
	}
	if cached.hasClient() && cached.Issuer == asm.Issuer && cached.RedirectURI == redirectURI {
		return cached.ClientID, cached.ClientSecret, nil
	}
	if asm.RegistrationEndpoint == "" {
		return "", "", fmt.Errorf("server %q requires a registered OAuth client and does not support dynamic registration; add one to mcp.json under \"oauth\": {\"clientID\": ...}", h.serverName)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reg, err := oauthex.RegisterClient(rctx, asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		RedirectURIs:            []string{redirectURI},
		TokenEndpointAuthMethod: "none", // public client; no secret to store
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		ClientName:              "o (Ollama agent harness)",
	}, h.httpClient)
	if err != nil {
		return "", "", fmt.Errorf("dynamic client registration with %s failed: %w", asm.Issuer, err)
	}
	// cache the registration immediately: a retry after an abandoned consent
	// reuses it instead of minting a new client each run
	h.cache.update(h.serverName, func(e *oauthEntry) {
		e.ServerURL = h.serverURL
		e.Issuer = asm.Issuer
		e.TokenEndpoint = asm.TokenEndpoint
		e.ClientID = reg.ClientID
		e.ClientSecret = reg.ClientSecret
		e.RedirectURI = redirectURI
	})
	return reg.ClientID, reg.ClientSecret, nil
}

// consentFetcher runs the browser-consent half of the flow: surface the URL,
// open the browser, wait for the loopback callback.
//
// RFC 9207 iss handling: some real-world servers (Mintlify) return an iss
// parameter without advertising "authorization_response_iss_parameter_supported"
// in metadata, which the SDK rejects outright. We preserve mix-up protection
// ourselves: verify a returned iss against the discovered issuer, and only
// pass iss through to the SDK when the server advertised support (in which
// case the SDK validates it).
func (h *oauthHandler) consentFetcher(listener *callbackListener, asm *oauthex.AuthServerMeta) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		// Adopt the state the SDK embedded in the authorization URL so the
		// callback listener can enforce it (defense in depth: the SDK also
		// validates it).
		u, err := url.Parse(args.URL)
		if err != nil {
			return nil, fmt.Errorf("invalid authorization URL from server: %w", err)
		}
		state := u.Query().Get("state")
		if state == "" {
			return nil, fmt.Errorf("authorization URL has no state parameter")
		}
		listener.expectState(state)

		fmt.Fprintf(h.consentOut, "\nmcp server %q needs authorization.\nOpen this URL to approve:\n\n    %s\n\n", h.serverName, args.URL)
		if h.open != nil {
			h.open(args.URL)
		} else {
			fmt.Fprintln(h.consentOut, "(browser auto-open disabled; open the URL manually)")
		}
		cctx, cancel := context.WithTimeout(ctx, consentTimeout)
		defer cancel()
		res, err := listener.wait(cctx)
		if err != nil {
			return nil, err
		}
		iss := res.iss
		if asm.AuthorizationResponseIssParameterSupported {
			// SDK validates iss itself in this case; pass it through.
			return &auth.AuthorizationResult{Code: res.code, State: res.state, Iss: iss}, nil
		}
		if iss != "" && iss != asm.Issuer {
			return nil, fmt.Errorf("authorization response issuer %q does not match expected issuer %q (possible mix-up attack)", iss, asm.Issuer)
		}
		// Issuer verified (or absent): strip so the SDK's strict
		// advertised-support check does not reject a working server.
		return &auth.AuthorizationResult{Code: res.code, State: res.state}, nil
	}
}

func clientCredentials(id, secret, issuer string) *oauthex.ClientCredentials {
	creds := &oauthex.ClientCredentials{ClientID: id, Issuer: issuer}
	if secret != "" {
		creds.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
	}
	return creds
}

// wellKnownResourceMetadataURL derives the RFC 9728 well-known URL when the
// server didn't advertise one in WWW-Authenticate.
func wellKnownResourceMetadataURL(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return serverURL
	}
	path := strings.TrimSuffix(u.Path, "/")
	u.Path = "/.well-known/oauth-protected-resource" + path
	u.RawQuery, u.Fragment = "", ""
	return u.String()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
