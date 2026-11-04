package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/keyring"
	"github.com/metafates/zemi/internal/mcp/mcptest"
)

// noKeychain keeps every test off the real OS keychain: grants exercise the
// file fallback.
type noKeychain struct{}

var errNoKeychain = errors.New("no keychain in tests")

func (noKeychain) Get(string, string) (string, error) { return "", keyring.ErrNotFound }
func (noKeychain) Set(string, string, string) error   { return errNoKeychain }
func (noKeychain) Delete(string, string) error        { return keyring.ErrNotFound }

func init() { keyring.Chain = noKeychain{} }

// dropIdleConns: the flow's browser and token calls share the process-wide
// transport, whose idle connection would otherwise outlive the test.
func dropIdleConns(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { http.DefaultClient.CloseIdleConnections() })
}

// TestOAuthSignIn drives the whole flow against a fake server: the 401
// points at resource metadata, zemi registers, the "browser" follows the
// authorization redirect to the callback, PKCE and resource are checked at
// the token endpoint, and a short-lived token refreshes on the next use.
func TestOAuthSignIn(t *testing.T) {
	var (
		mu                 sync.Mutex
		challenge, current string
		refreshes          int
	)
	var srv *httptest.Server
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.MarshalWrite(w, v, json.Deterministic(true))
	}
	mux := http.NewServeMux()
	mcpHandler := mcptest.Handler()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := current != "" && r.Header.Get("Authorization") == "Bearer "+current
		mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL + "/auth"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/auth", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"issuer": srv.URL + "/auth", "authorization_endpoint": srv.URL + "/auth/authorize",
			"token_endpoint": srv.URL + "/auth/token", "registration_endpoint": srv.URL + "/auth/register",
		})
	})
	mux.HandleFunc("/auth/register", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, map[string]any{"client_id": "c1"})
	})
	mux.HandleFunc("/auth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "c1" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != srv.URL+"/mcp" {
			http.Error(w, "bad authorize request "+q.Encode(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		challenge = q.Get("code_challenge")
		mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=abc&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		if r.Form.Get("resource") != srv.URL+"/mcp" || r.Form.Get("client_id") != "c1" {
			writeJSON(w, 400, map[string]any{"error": "invalid_request"})
			return
		}
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("code") != "abc" {
				writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
				return
			}
			current = "t1"
			writeJSON(w, 200, map[string]any{"access_token": "t1", "refresh_token": "r1", "expires_in": 1})
		case "refresh_token":
			refreshes++
			current = "t2"
			writeJSON(w, 200, map[string]any{"access_token": "t2", "expires_in": 3600})
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	dropIdleConns(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "fake.json")
	auth := &Auth{Name: "fake", ServerURL: srv.URL + "/mcp", Path: path}
	if _, err := Start(ctx, Config{URL: auth.ServerURL, Auth: auth}); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("start before sign-in: %v", err)
	}

	page := make(chan string, 1)
	err := auth.Authorize(ctx, func(u string) {
		go func() {
			resp, err := http.Get(u)
			if err != nil {
				page <- err.Error()
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			page <- string(b)
		}()
	})
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	if p := <-page; !strings.Contains(p, "<h1>zemi @^^</h1>") || !strings.Contains(p, "<b>fake</b>") {
		t.Fatalf("callback page:\n%s", p)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}

	// a fresh Auth reads the stored grant; the 1s token refreshes first
	auth = &Auth{Name: "fake", ServerURL: srv.URL + "/mcp", Path: path}
	c, err := Start(ctx, Config{URL: auth.ServerURL, Auth: auth})
	if err != nil {
		t.Fatalf("start after sign-in: %v", err)
	}
	defer c.Close()
	if _, err := c.Tools(ctx); err != nil {
		t.Fatalf("tools: %v", err)
	}
	mu.Lock()
	n := refreshes
	mu.Unlock()
	if n != 1 {
		t.Fatalf("refreshes = %d, want 1", n)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"t2"`) {
		t.Fatalf("refreshed token not saved: %s", b)
	}
}

// TestOAuthRefreshRotation: two windows share one grant with a server that
// takes each refresh token once; each refreshes from the newest stored one.
func TestOAuthRefreshRotation(t *testing.T) {
	var (
		mu sync.Mutex
		n  int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		if r.Form.Get("refresh_token") != fmt.Sprint("r", n) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.MarshalWrite(w, map[string]any{"error": "invalid_grant"}, json.Deterministic(true))
			return
		}
		n++
		// expiring at once, so every call refreshes
		_ = json.MarshalWrite(w, map[string]any{"access_token": fmt.Sprint("a", n), "refresh_token": fmt.Sprint("r", n), "expires_in": 1}, json.Deterministic(true))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "grant.json")
	grant := &Auth{ServerURL: "https://mcp", Path: path}
	if err := grant.save(&Token{ServerURL: "https://mcp", AccessToken: "a0", RefreshToken: "r0", Expiry: time.Now(), ClientID: "c", TokenURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	windows := []*Auth{{ServerURL: "https://mcp", Path: path}, {ServerURL: "https://mcp", Path: path}}
	for _, w := range windows {
		w.mu.Lock()
		w.load()
		w.mu.Unlock()
	}
	for i := range 4 {
		if _, err := windows[i%2].bearer(t.Context()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

// TestOAuthPreRegisteredClient: a server with no dynamic client
// registration says so, and takes a client_id set in the config instead.
func TestOAuthPreRegisteredClient(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server/mcp", func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, map[string]any{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token", "registration_endpoint": srv.URL + "/register",
		}, json.Deterministic(true))
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "mine" {
			http.Error(w, "client_id "+q.Get("client_id"), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=abc&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, map[string]any{"access_token": "t1", "expires_in": 3600}, json.Deterministic(true))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	dropIdleConns(t)

	visit := func(u string) {
		go func() {
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
			}
		}()
	}
	auth := &Auth{Name: "fake", ServerURL: srv.URL + "/mcp", Path: filepath.Join(t.TempDir(), "a.json")}
	err := auth.Authorize(t.Context(), visit)
	if err == nil || !strings.Contains(err.Error(), "dynamic client registration") {
		t.Fatalf("without a client id: %v", err)
	}
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	auth = &Auth{Name: "fake", ServerURL: srv.URL + "/mcp", Path: filepath.Join(t.TempDir(), "b.json"), ClientID: "mine", CallbackPort: port}
	if err := auth.Authorize(t.Context(), visit); err != nil {
		t.Fatalf("with a client id: %v", err)
	}
	if want := fmt.Sprintf("http://localhost:%d/callback", port); auth.tok.RedirectURI != want {
		t.Fatalf("redirect %q, want %q", auth.tok.RedirectURI, want)
	}
}

func TestCallbackPageEscapes(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCallbackPage(rec, "<srv>", errors.New(`<script>alert(1)</script>`))
	if body := rec.Body.String(); strings.Contains(body, "<script>alert") || rec.Code != 400 {
		t.Fatalf("unescaped failure page (%d):\n%s", rec.Code, body)
	}
}

// TestCallbackPageMatchesLanding: callback.html copies the landing's palette,
// since go:embed cannot reach docs/.
func TestCallbackPageMatchesLanding(t *testing.T) {
	style, err := os.ReadFile("../../docs/style.css")
	if err != nil {
		t.Fatal(err)
	}
	// every color token the landing's palette defines and the callback
	// carries is the same: var/value pairs from docs/style.css's :root
	_, root, _ := strings.Cut(string(style), ":root {")
	root, _, _ = strings.Cut(root, "}")
	for line := range strings.SplitSeq(root, "\n") {
		pair := strings.TrimSpace(line)
		if !strings.HasPrefix(pair, "--") || !strings.Contains(pair, ": #") {
			continue // not a color token (color-scheme, the fonts)
		}
		if !strings.Contains(callbackHTML, strings.TrimSuffix(pair, ";")) {
			t.Errorf("palette: %q is not on the callback page", pair)
		}
	}
}

func TestCheckEndpoint(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://auth.example.com/authorize": true,
		"http://localhost:8080/token":        true,
		"http://auth.example.com/token":      false,
		"file:///etc/passwd":                 false,
	} {
		if got := checkEndpoint(raw) == nil; got != ok {
			t.Errorf("checkEndpoint(%q) ok=%v", raw, got)
		}
	}
}

// TestDiscoverChecksResource: metadata naming another resource is ignored,
// so zemi never asks for a token meant for that resource on its behalf.
func TestDiscoverChecksResource(t *testing.T) {
	var (
		srv      *httptest.Server
		resource atomic.Value
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, map[string]any{"resource": resource.Load(), "authorization_servers": []string{srv.URL + "/as"}}, json.Deterministic(true))
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/as", func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, map[string]any{"authorization_endpoint": srv.URL + "/as/authorize", "token_endpoint": srv.URL + "/as/token"}, json.Deterministic(true))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	for res, ok := range map[string]bool{
		"":                           true,
		srv.URL + "/mcp":             true,
		srv.URL:                      true,
		"https://victim.example/mcp": false,
		srv.URL + "/mcp-other":       false,
		srv.URL + "/mcp/deeper":      false,
		"https" + strings.TrimPrefix(srv.URL, "http") + "/mcp": false,
	} {
		resource.Store(res)
		m, err := discover(t.Context(), srv.URL+"/mcp")
		if err != nil {
			t.Fatalf("%q: %v", res, err)
		}
		if got := m.authURL == srv.URL+"/as/authorize"; got != ok {
			t.Errorf("resource %q: used the metadata = %v, want %v", res, got, ok)
		}
	}
}

// TestDiscoverWithoutAuthServers: a protected-resource document that names
// no authorization server still says what the token is for and what to ask
// for, and the resource is then its own authorization server (RFC 9728).
// The challenge may point at that document with a path rather than a URI,
// and a server with no metadata at all still gets the RFC 8707 resource.
func TestDiscoverWithoutAuthServers(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="/.well-known/oauth-protected-resource/mcp"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, map[string]any{"resource": srv.URL + "/mcp", "scopes_supported": []string{"read", "write"}}, json.Deterministic(true))
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		_ = json.MarshalWrite(w, map[string]any{"authorization_endpoint": srv.URL + "/authorize", "token_endpoint": srv.URL + "/token"}, json.Deterministic(true))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	m, err := discover(t.Context(), srv.URL+"/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if m.authURL != srv.URL+"/authorize" || m.resource != srv.URL+"/mcp" || m.scope != "read write" {
		t.Fatalf("discover = %+v", m)
	}

	// no metadata anywhere: the endpoints are the spec's guess and the
	// resource is still the server's canonical URI
	bare := httptest.NewServer(http.NotFoundHandler())
	defer bare.Close()
	if m, err = discover(t.Context(), bare.URL+"/mcp/"); err != nil {
		t.Fatal(err)
	}
	if m.resource != bare.URL+"/mcp" || m.registerURL != bare.URL+"/register" {
		t.Fatalf("bare server: %+v", m)
	}
}
