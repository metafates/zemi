package mcp

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/keyring"
)

// noKeychain keeps every test off the real OS keychain: grants exercise the
// file fallback.
type noKeychain struct{}

var errNoKeychain = errors.New("no keychain in tests")

func (noKeychain) Get(string, string) (string, error) { return "", keyring.ErrNotFound }
func (noKeychain) Set(string, string, string) error   { return errNoKeychain }
func (noKeychain) Delete(string, string) error        { return keyring.ErrNotFound }

func init() { keyring.Chain = noKeychain{} }

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
		if _, err := windows[i%2].grant(t.Context()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

// TestOAuthPreRegisteredClient: a server with no dynamic client
// registration says so, and takes a client_id set in the config instead.
func TestOAuthPreRegisteredClient(t *testing.T) {
	var srv *httptest.Server
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, v, json.Deterministic(true))
	}
	mkMux := func(registerOK bool) *http.ServeMux {
		mux := http.NewServeMux()
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "sign in", http.StatusUnauthorized)
		})
		mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL}})
		})
		mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize",
				"token_endpoint": srv.URL + "/token", "registration_endpoint": srv.URL + "/register",
				"code_challenge_methods_supported": []string{"S256"},
			})
		})
		mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
			if !registerOK {
				http.NotFound(w, r)
				return
			}
			writeJSON(w, map[string]any{"client_id": "c1"})
		})
		mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("client_id") != "mine" {
				http.Error(w, "client_id "+q.Get("client_id"), http.StatusBadRequest)
				return
			}
			http.Redirect(w, r, q.Get("redirect_uri")+"?code=abc&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
		})
		mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"access_token": "t1", "expires_in": 3600})
		})
		return mux
	}
	srv = httptest.NewServer(mkMux(false))
	defer srv.Close()
	t.Cleanup(func() { http.DefaultClient.CloseIdleConnections() })

	visit := func(u string) {
		go func() {
			if resp, err := http.Get(u); err == nil {
				resp.Body.Close()
			}
		}()
	}
	auth := &Auth{Name: "fake", ServerURL: srv.URL + "/mcp", Path: filepath.Join(t.TempDir(), "a.json")}
	err := auth.Authorize(t.Context(), visit)
	if err == nil || !strings.Contains(err.Error(), "register") {
		t.Fatalf("without a client id: %v", err)
	}
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listen address = %T, want *net.TCPAddr", ln.Addr())
	}
	port := addr.Port
	ln.Close()
	srv2 := httptest.NewServer(mkMux(true))
	defer srv2.Close()
	srv = srv2
	auth = &Auth{Name: "fake", ServerURL: srv.URL + "/mcp", Path: filepath.Join(t.TempDir(), "b.json"), ClientID: "mine", CallbackPort: port}
	if err := auth.Authorize(t.Context(), visit); err != nil {
		t.Fatalf("with a client id: %v", err)
	}
	if want := fmt.Sprintf("http://localhost:%d/callback", port); auth.tok.RedirectURI != want {
		t.Fatalf("redirect %q, want %q", auth.tok.RedirectURI, want)
	}
}
