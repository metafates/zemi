// oauth.go signs zemi in to HTTP MCP servers that answer 401. Discovery,
// PKCE, dynamic client registration and the token exchange are the go-sdk's
// auth package; what stays here is the grant's persistence (the OS keychain
// with a 0600 file fallback), its cross-process refresh, the passive handler
// the transport gets, and the browser flow /mcp's sign-in runs.

package mcp

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/metafates/zemi/internal/filelock"
	"github.com/metafates/zemi/internal/keyring"
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

// Auth holds the grant for the server at ServerURL; Path is the fallback
// file's path for when no OS keychain answers.
type Auth struct {
	Name      string // shown on the browser page
	ServerURL string
	Path      string
	DataHome  string // the tree's data home: the keychain account's scope
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

// _tokenLockWait bounds one process's wait for the grant's refresh lock: the
// holder is a refresh call, bounded by tokenHTTP's own timeout, so a wait
// past it means a wedged holder and the caller refreshes unlocked.
const _tokenLockWait = 35 * time.Second

// _tokenHTTP never follows a redirect: a token endpoint answers, and a 307
// would re-send the grant and the client secret wherever it points.
var _tokenHTTP = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

// _oauthHTTP does the flow's own HTTP: the 401 provoke and the SDK
// handler's discovery, registration and exchange.
var _oauthHTTP = &http.Client{Timeout: 30 * time.Second}

// Ref is the grant's store: the OS keychain scoped to the tree's data home
// (the account carries the scope, so a lab cannot touch the real install's
// grant), the file as the fallback.
func (a *Auth) Ref() keyring.Ref {
	return keyring.Scoped(a.DataHome, "mcp:"+a.Name, a.Path)
}

// load reads the stored grant once; a grant for another URL is ignored.
// The caller holds mu.
func (a *Auth) load() *Token {
	if !a.loaded {
		a.loaded = true
		if s, _, ok := a.Ref().Get(); ok {
			var t Token
			if json.Unmarshal([]byte(s), &t) == nil && t.ServerURL == a.ServerURL && t.AccessToken != "" {
				a.tok = &t
			}
		}
	}
	return a.tok
}

// save writes the grant, the keychain first. The caller holds mu.
func (a *Auth) save(t *Token) error {
	a.tok, a.loaded = t, true
	b, err := json.Marshal(t, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = a.Ref().Set(string(b))
	return err
}

// Logout forgets the grant.
func (a *Auth) Logout() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok, a.loaded = nil, true
	return a.Ref().Delete()
}

func fresh(t *Token) bool { return t.Expiry.IsZero() || time.Until(t.Expiry) > 30*time.Second }

// grant returns the stored grant, refreshing one about to expire; nil when
// there is no grant. Another zemi may have refreshed already, and a server
// that rotates refresh tokens refuses the one it used: the refresh runs
// under a lock on the grant, so one process refreshes and the rest re-read
// its grant. A lock that cannot be taken leaves the old race.
func (a *Auth) grant(ctx context.Context) (*Token, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.load()
	if t == nil {
		return nil, nil
	}
	if fresh(t) {
		return t, nil
	}
	if a.Path != "" {
		if unlock, err := filelock.Lock(a.Path+".lock", _tokenLockWait); err == nil {
			defer unlock()
		}
	}
	a.loaded = false
	switch ft := a.load(); {
	case ft == nil:
		return nil, nil
	case ft.AccessToken == t.AccessToken:
		a.tok = t // a token reject marked expired stays expired
	case fresh(ft):
		return ft, nil
	default:
		t = ft
	}
	if t.RefreshToken == "" {
		// no way back to a token: the request goes on unsigned and the 401
		// asks for a sign-in
		return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	}
	nt, err := tokenRequest(ctx, t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}})
	if err != nil {
		return nil, err
	}
	if err := a.save(nt); err != nil {
		return nil, err
	}
	return nt, nil
}

// grantSource serves the grant as the oauth2.TokenSource the SDK's transport
// asks on every request. It has no context (oauth2.TokenSource), so the
// refresh runs on a background one.
type grantSource struct{ a *Auth }

func (g grantSource) Token() (*oauth2.Token, error) {
	t, err := g.a.grant(context.Background())
	if err != nil || t == nil {
		return nil, err
	}
	return &oauth2.Token{
		AccessToken:  t.AccessToken,
		TokenType:    "Bearer",
		RefreshToken: t.RefreshToken,
		Expiry:       t.Expiry,
	}, nil
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

// passiveHandler is the OAuthHandler the transport gets: it serves the
// stored grant and, on a refusal, refreshes once. It never discovers,
// registers or opens a browser behind the model's back; that is the
// explicit sign-in's job.
type passiveHandler struct{ a *Auth }

func (p passiveHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return grantSource(p), nil
}

func (p passiveHandler) Authorize(_ context.Context, req *http.Request, resp *http.Response) error {
	resp.Body.Close()
	bearer := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !p.a.reject(bearer) {
		return ErrAuthRequired
	}
	return nil // refreshable: the transport retries and Token serves the fresh grant
}

// tokenRequest posts to the grant's token endpoint and returns the updated
// grant; it is the refresh path. A refusal wraps ErrAuthRequired; a network
// failure does not.
func tokenRequest(ctx context.Context, t *Token, form url.Values) (*Token, error) {
	if t.Resource != "" {
		form.Set("resource", t.Resource)
	}
	if t.ClientSecret == "" {
		form.Set("client_id", t.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, redactAll(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if t.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(t.ClientID), url.QueryEscape(t.ClientSecret))
	}
	resp, err := _tokenHTTP.Do(req)
	if err != nil {
		return nil, redactAll(err)
	}
	defer resp.Body.Close()
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		Description  string `json:"error_description"`
	}
	_ = json.UnmarshalRead(io.LimitReader(resp.Body, 1<<20), &r)
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

// Authorize runs the browser sign-in and stores the grant. open receives
// the authorization URL to show the user and open in a browser. The
// context bounds the whole flow, the wait for the user included.
func (a *Auth) Authorize(ctx context.Context, open func(authURL string)) error {
	// the same throwaway initialize the transport's own 401 is: the
	// challenge names the resource metadata the handler discovers from
	req, resp, err := a.provoke(ctx)
	if err != nil {
		return err
	}

	ln, err := listenCallback(a.CallbackPort)
	if err != nil {
		return err
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return errors.New("the sign-in callback did not bind a local TCP port")
	}
	redirect := fmt.Sprintf("http://localhost:%d/callback", addr.Port)

	id, secret := a.ClientID, a.ClientSecret
	if id == "" {
		// a grant from an earlier sign-in carries the client the server knows
		a.mu.Lock()
		if prev := a.load(); prev != nil && prev.ClientID != "" {
			id, secret = prev.ClientID, prev.ClientSecret
		}
		a.mu.Unlock()
	}
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirect,
		AuthorizationCodeFetcher: a.fetcher(ln, open),
		RequestRefreshToken:      true,
		Client:                   _oauthHTTP,
	}
	if id != "" {
		cc := &oauthex.ClientCredentials{ClientID: id}
		if secret != "" {
			cc.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
		}
		cfg.PreregisteredClient = cc
	} else {
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{
				ClientName:              "zemi",
				RedirectURIs:            []string{redirect},
				GrantTypes:              []string{"authorization_code", "refresh_token"},
				ResponseTypes:           []string{"code"},
				TokenEndpointAuthMethod: "none",
				ApplicationType:         "native",
			},
		}
	}
	// the exchanged grant lands in the keyring, and the wrapper serves it
	cfg.NewTokenSource = func(_ context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		nt := &Token{
			ServerURL: a.ServerURL, AccessToken: tok.AccessToken, RefreshToken: tok.RefreshToken,
			Expiry: tok.Expiry, ClientID: oc.ClientID, ClientSecret: oc.ClientSecret,
			RedirectURI: redirect, AuthURL: oc.Endpoint.AuthURL, TokenURL: oc.Endpoint.TokenURL,
		}
		a.mu.Lock()
		err := a.save(nt)
		a.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return grantSource{a}, nil
	}
	handler, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return err
	}
	if err := handler.Authorize(ctx, req, resp); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.load()
	if t == nil {
		return errors.New("the sign-in finished without a grant")
	}
	return nil
}

// provoke sends the throwaway initialize that makes a protected server say
// who signs in: the 401's challenge drives the handler's discovery.
func (a *Auth) provoke(ctx context.Context) (*http.Request, *http.Response, error) {
	init := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"` + ProtocolVersion + `","capabilities":{},"clientInfo":{"name":"zemi","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.ServerURL, strings.NewReader(init))
	if err != nil {
		return nil, nil, redactAll(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := _oauthHTTP.Do(req)
	if err != nil {
		return nil, nil, redactAll(err)
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		return nil, nil, errors.New("the server did not ask for a sign-in")
	}
	return req, resp, nil
}

// fetcher is the AuthorizationCodeFetcher, the browser's home: the handler
// builds the authorization URL, the state and the PKCE verifier, and
// validates the state it gets back; this serves /callback, opens the
// browser and returns what the redirect carried.
func (a *Auth) fetcher(ln net.Listener, open func(authURL string)) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		done := make(chan *auth.AuthorizationResult, 1)
		errc := make(chan error, 1)
		srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cq := r.URL.Query()
			if r.URL.Path != "/callback" {
				http.NotFound(w, r) // a stray request must not end the flow
				return
			}
			var err error
			switch {
			case cq.Get("error") != "":
				err = fmt.Errorf("the server refused: %s", cmp.Or(cq.Get("error_description"), cq.Get("error")))
			case cq.Get("code") == "":
				err = errors.New("the server sent no authorization code")
			}
			writeCallbackPage(w, a.Name, err)
			if err != nil {
				errc <- err
				return
			}
			select {
			case done <- &auth.AuthorizationResult{Code: cq.Get("code"), State: cq.Get("state"), Iss: cq.Get("iss")}:
			default:
			}
		})}
		go srv.Serve(ln)
		// Shutdown, not Close: it joins the callback's handler goroutines, so
		// the flow never returns while the browser's page is still being written.
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
		open(args.URL)
		select {
		case res := <-done:
			return res, nil
		case err := <-errc:
			return nil, err
		case <-ctx.Done():
			return nil, fmt.Errorf("sign-in did not finish: %w", ctx.Err())
		}
	}
}

// _callbackPorts are tried in order so the registered redirect URI stays
// stable between sign-ins; any free port is the last resort.
var _callbackPorts = []int{40704, 40705, 40706, 40707, 40708, 40709, 40710, 40711, 40712, 40713, 0}

func listenCallback(fixed int) (net.Listener, error) {
	ports := _callbackPorts
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
	_ = _callbackPage.Execute(w, data)
}

//go:embed callback.html
var _callbackHTML string

var _callbackPage = template.Must(template.New("callback").Parse(_callbackHTML))
