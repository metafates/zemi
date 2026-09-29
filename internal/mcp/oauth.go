// oauth.go signs zemi in to HTTP MCP servers that answer 401, following
// the MCP authorization spec: protected resource metadata (RFC 9728),
// authorization server metadata (RFC 8414 or OpenID discovery), dynamic
// client registration (RFC 7591), and the authorization code flow with
// PKCE through a localhost callback. Grants persist per server and refresh
// on their own.

package mcp

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/metafates/zemi/internal/config"
)

// ErrAuthRequired means the server wants a sign-in zemi does not have (or
// can no longer refresh).
var ErrAuthRequired = errors.New("sign-in required")

// Token is one server's stored grant with what refreshing it needs.
type Token struct {
	ServerURL    string    `json:"server_url"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitzero"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	RedirectURI  string    `json:"redirect_uri"`
	AuthURL      string    `json:"auth_url"`
	TokenURL     string    `json:"token_url"`
	Resource     string    `json:"resource,omitempty"`
}

// Auth holds the grant for the server at ServerURL, persisted at Path.
type Auth struct {
	Name      string // shown on the browser page
	ServerURL string
	Path      string
	// ClientID and ClientSecret are a client registered with the server by
	// hand; empty means register on the fly (RFC 7591).
	ClientID     string
	ClientSecret string
	// CallbackPort pins the redirect port for a server that matches the
	// redirect URI exactly; 0 tries callbackPorts.
	CallbackPort int

	mu     sync.Mutex
	tok    *Token
	loaded bool
}

var oauthHTTP = &http.Client{Timeout: 30 * time.Second}

// tokenHTTP never follows a redirect: a token endpoint answers, and a 307
// would re-send the grant and the client secret wherever it points.
var tokenHTTP = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

// load reads the stored grant once; a grant for another URL is ignored.
// The caller holds mu.
func (a *Auth) load() *Token {
	if !a.loaded {
		a.loaded = true
		if b, err := os.ReadFile(a.Path); err == nil {
			var t Token
			if json.Unmarshal(b, &t) == nil && t.ServerURL == a.ServerURL && t.AccessToken != "" {
				a.tok = &t
			}
		}
	}
	return a.tok
}

// save writes the grant owner-only. The caller holds mu.
func (a *Auth) save(t *Token) error {
	a.tok, a.loaded = t, true
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(a.Path, b, 0o600)
}

// Logout forgets the grant.
func (a *Auth) Logout() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok, a.loaded = nil, true
	if err := os.Remove(a.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// bearer returns the access token, refreshing one about to expire; "" when
// there is no grant.
func (a *Auth) bearer(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.load()
	if t == nil {
		return "", nil
	}
	fresh := func(t *Token) bool { return t.Expiry.IsZero() || time.Until(t.Expiry) > 30*time.Second }
	if fresh(t) {
		return t.AccessToken, nil
	}
	// another window may have refreshed already, and a server that rotates
	// refresh tokens refuses the one it used.
	// NOTE: two processes refreshing at the same moment still race; an flock
	// on the grant file around the refresh would close that.
	a.loaded = false
	switch ft := a.load(); {
	case ft.AccessToken == t.AccessToken:
		a.tok = t // a token reject marked expired stays expired
	case fresh(ft):
		return ft.AccessToken, nil
	default:
		t = ft
	}
	if t.RefreshToken == "" {
		return "", ErrAuthRequired
	}
	nt, err := tokenRequest(ctx, t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}})
	if err != nil {
		return "", err
	}
	if err := a.save(nt); err != nil {
		return "", err
	}
	return nt.AccessToken, nil
}

// reject marks an access token the server refused as expired and reports
// whether another try can succeed (a refresh token, or a newer token).
func (a *Auth) reject(access string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.load()
	switch {
	case t == nil:
		return false
	case t.AccessToken != access:
		return true
	case t.RefreshToken == "":
		return false
	}
	t.Expiry = time.Now().Add(-time.Minute)
	return true
}

// Authorize runs the browser sign-in and stores the grant. open receives
// the authorization URL to show the user and open in a browser. The
// context bounds the whole flow, the wait for the user included.
func (a *Auth) Authorize(ctx context.Context, open func(authURL string)) error {
	m, err := discover(ctx, a.ServerURL)
	if err != nil {
		return err
	}
	ln, err := listenCallback(a.CallbackPort)
	if err != nil {
		return err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://localhost:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	t := &Token{ServerURL: a.ServerURL, RedirectURI: redirect, AuthURL: m.authURL, TokenURL: m.tokenURL, Resource: m.resource}
	t.ClientID, t.ClientSecret = a.ClientID, a.ClientSecret
	if t.ClientID == "" {
		a.mu.Lock()
		if prev := a.load(); prev != nil && prev.RedirectURI == redirect && prev.TokenURL == m.tokenURL {
			t.ClientID, t.ClientSecret = prev.ClientID, prev.ClientSecret
		}
		a.mu.Unlock()
	}
	if t.ClientID == "" {
		if m.registerURL == "" {
			return errors.New("the server does not let apps register; set an Authorization header in its config instead")
		}
		if t.ClientID, t.ClientSecret, err = register(ctx, m.registerURL, redirect); err != nil {
			return err
		}
	}

	verifier := randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	state := randomString(16)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {t.ClientID},
		"redirect_uri":          {redirect},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	q.Set("resource", m.resource)
	if m.scope != "" {
		q.Set("scope", m.scope)
	}
	sep := "?"
	if strings.Contains(m.authURL, "?") {
		sep = "&"
	}

	done := make(chan error, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cq := r.URL.Query()
		if r.URL.Path != "/callback" || cq.Get("state") != state {
			http.NotFound(w, r) // a stray request must not end the flow
			return
		}
		var err error
		switch {
		case cq.Get("error") != "":
			err = fmt.Errorf("the server refused: %s", cmp.Or(cq.Get("error_description"), cq.Get("error")))
		case cq.Get("code") == "":
			err = errors.New("the server sent no authorization code")
		default:
			var nt *Token
			nt, err = tokenRequest(ctx, t, url.Values{"grant_type": {"authorization_code"}, "code": {cq.Get("code")}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
			if err == nil {
				a.mu.Lock()
				err = a.save(nt)
				a.mu.Unlock()
			}
		}
		writeCallbackPage(w, a.Name, err)
		select {
		case done <- err:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	open(m.authURL + sep + q.Encode())
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("sign-in did not finish: %w", ctx.Err())
	}
}

type authMeta struct {
	authURL, tokenURL, registerURL, resource, scope string
}

var challengeParam = regexp.MustCompile(`([a-zA-Z_]+)="([^"]*)"`)

// discover finds where to sign in: the 401 challenge or the well-known
// resource metadata names the authorization server, whose metadata names
// the endpoints. A server without metadata is its own authorization server
// at the default paths (the 2025-03-26 spec).
func discover(ctx context.Context, serverURL string) (*authMeta, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, redactURL(err)
	}
	origin := u.Scheme + "://" + u.Host
	// RFC 8707: the token is bound to the server's canonical URI, and the MCP
	// spec has the client send it whether or not metadata named one.
	m := &authMeta{resource: origin + strings.TrimSuffix(u.EscapedPath(), "/")}

	var prmURLs []string
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"` + ProtocolVersion + `","capabilities":{},"clientInfo":{"name":"zemi","version":"1"}}}`
	if req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(init)); err == nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if resp, err := oauthHTTP.Do(req); err == nil {
			resp.Body.Close()
			for _, h := range resp.Header.Values("WWW-Authenticate") {
				for _, p := range challengeParam.FindAllStringSubmatch(h, -1) {
					switch p[1] {
					case "resource_metadata":
						// servers do send a path here, though RFC 9728 wants a URI
						if ref, err := u.Parse(p[2]); err == nil {
							prmURLs = append(prmURLs, ref.String())
						}
					case "scope":
						m.scope = p[2]
					}
				}
			}
		}
	}
	if path := strings.TrimSuffix(u.EscapedPath(), "/"); path != "" {
		prmURLs = append(prmURLs, origin+"/.well-known/oauth-protected-resource"+path)
	}
	prmURLs = append(prmURLs, origin+"/.well-known/oauth-protected-resource")

	issuer := origin
	for _, pu := range prmURLs {
		var prm struct {
			Resource    string   `json:"resource"`
			AuthServers []string `json:"authorization_servers"`
			Scopes      []string `json:"scopes_supported"`
		}
		// a resource other than this server would have its token sent here (RFC 9728 3.3)
		if getJSON(ctx, pu, &prm) != nil || (prm.Resource != "" && !coversServer(prm.Resource, u)) {
			continue
		}
		// authorization_servers is optional: without it the resource is its
		// own authorization server, and the document still names the audience
		// and the scopes to ask for
		if len(prm.AuthServers) > 0 {
			issuer = prm.AuthServers[0]
		}
		m.resource = cmp.Or(prm.Resource, m.resource)
		if m.scope == "" {
			m.scope = strings.Join(prm.Scopes, " ")
		}
		break
	}

	iu, err := url.Parse(issuer)
	if err != nil {
		return nil, fmt.Errorf("bad authorization server %q: %w", issuer, err)
	}
	iorigin, ipath := iu.Scheme+"://"+iu.Host, strings.TrimSuffix(iu.EscapedPath(), "/")
	cands := []string{iorigin + "/.well-known/oauth-authorization-server" + ipath, iorigin + "/.well-known/openid-configuration" + ipath}
	if ipath != "" {
		cands = append(cands, iorigin+ipath+"/.well-known/openid-configuration")
	}
	for _, c := range cands {
		var asm struct {
			Authorization string `json:"authorization_endpoint"`
			Token         string `json:"token_endpoint"`
			Registration  string `json:"registration_endpoint"`
		}
		if getJSON(ctx, c, &asm) == nil && asm.Authorization != "" && asm.Token != "" {
			m.authURL, m.tokenURL, m.registerURL = asm.Authorization, asm.Token, asm.Registration
			break
		}
	}
	if m.authURL == "" {
		m.authURL, m.tokenURL, m.registerURL = iorigin+"/authorize", iorigin+"/token", iorigin+"/register"
	}
	for _, e := range []string{m.authURL, m.tokenURL, m.registerURL} {
		if err := checkEndpoint(e); e != "" && err != nil {
			return nil, err
		}
	}
	return m, nil
}

// coversServer reports whether a protected resource names the server: the
// same origin, and the server's path equal to or under the resource's.
func coversServer(resource string, server *url.URL) bool {
	r, err := url.Parse(resource)
	if err != nil {
		return false
	}
	rp, sp := strings.TrimSuffix(r.EscapedPath(), "/"), strings.TrimSuffix(server.EscapedPath(), "/")
	return strings.EqualFold(r.Scheme, server.Scheme) && strings.EqualFold(r.Host, server.Host) &&
		(rp == sp || strings.HasPrefix(sp, rp+"/"))
}

// checkEndpoint refuses endpoints that would send secrets in the clear or
// open something other than a web page: https, or http on this machine.
func checkEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if u.Scheme == "https" || (u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return nil
	}
	return fmt.Errorf("refusing sign-in endpoint %q: it must use https", raw)
}

func getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return redactURL(err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return redactURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// register asks the authorization server for a client id (RFC 7591), as a
// public native client that proves itself with PKCE.
func register(ctx context.Context, endpoint, redirect string) (id, secret string, err error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "zemi",
		"redirect_uris":              []string{redirect},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"application_type":           "native",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := oauthHTTP.Do(req)
	if err != nil {
		return "", "", redactURL(err)
	}
	defer resp.Body.Close()
	var r struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if resp.StatusCode/100 != 2 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r) != nil || r.ClientID == "" {
		// name the endpoint: a 404 here usually means discovery found no
		// metadata and this URL was the spec's guess
		return "", "", fmt.Errorf("registering zemi at %s failed (http %d); the server may not support dynamic client registration, so either register an app with it yourself and pass client_id (and client_secret, callback_port when its redirect URI has a fixed port) to zemi.mcp.add, or set an Authorization header in its config", endpoint, resp.StatusCode)
	}
	return r.ClientID, r.ClientSecret, nil
}

// tokenRequest posts to the token endpoint and returns the updated grant.
// A refusal wraps ErrAuthRequired; a network failure does not.
func tokenRequest(ctx context.Context, t *Token, form url.Values) (*Token, error) {
	if t.Resource != "" {
		form.Set("resource", t.Resource)
	}
	if t.ClientSecret == "" {
		form.Set("client_id", t.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, redactURL(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if t.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(t.ClientID), url.QueryEscape(t.ClientSecret))
	}
	resp, err := tokenHTTP.Do(req)
	if err != nil {
		return nil, redactURL(err)
	}
	defer resp.Body.Close()
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r)
	if resp.StatusCode != http.StatusOK || r.AccessToken == "" {
		why := cmp.Or(r.Description, r.Error, fmt.Sprintf("http %d", resp.StatusCode))
		return nil, fmt.Errorf("%w: the server refused the token request: %.200s", ErrAuthRequired, why)
	}
	nt := *t
	nt.AccessToken, nt.Expiry = r.AccessToken, time.Time{}
	if r.RefreshToken != "" {
		nt.RefreshToken = r.RefreshToken
	}
	if r.ExpiresIn > 0 {
		nt.Expiry = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	return &nt, nil
}

// callbackPorts are tried in order so the registered redirect URI stays
// stable between sign-ins; any free port is the last resort.
var callbackPorts = []int{40704, 40705, 40706, 40707, 40708, 40709, 40710, 40711, 40712, 40713, 0}

func listenCallback(fixed int) (net.Listener, error) {
	ports := callbackPorts
	if fixed > 0 {
		ports = []int{fixed}
	}
	var err error
	for _, p := range ports {
		var ln net.Listener
		if ln, err = net.Listen("tcp", fmt.Sprintf("localhost:%d", p)); err == nil {
			return ln, nil
		}
	}
	return nil, fmt.Errorf("no local port for the sign-in callback: %w", err)
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeCallbackPage(w http.ResponseWriter, server string, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data := struct {
		Server, Reason string
		OK             bool
	}{Server: server, OK: err == nil}
	if err != nil {
		data.Reason = err.Error()
		w.WriteHeader(http.StatusBadRequest)
	}
	_ = callbackPage.Execute(w, data)
}

//go:embed callback.html
var callbackHTML string

var callbackPage = template.Must(template.New("callback").Parse(callbackHTML))
