package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	"github.com/metafates/zemi/internal/mcp/mcptest"
)

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
		_ = json.NewEncoder(w).Encode(v)
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
		writeJSON(w, 200, map[string]any{"issuer": srv.URL + "/auth", "authorization_endpoint": srv.URL + "/auth/authorize",
			"token_endpoint": srv.URL + "/auth/token", "registration_endpoint": srv.URL + "/auth/register"})
	})
	mux.HandleFunc("/auth/register", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 201, map[string]any{"client_id": "c1"})
	})
	mux.HandleFunc("/auth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "c1" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != srv.URL+"/mcp" {
			http.Error(w, "bad authorize request "+q.Encode(), 400)
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	if p := <-page; !strings.Contains(p, "You're signed in") || !strings.Contains(p, "<b>fake</b>") {
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
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"t2"`) {
		t.Fatalf("refreshed token not saved: %s", b)
	}
}

func TestCallbackPageEscapes(t *testing.T) {
	rec := httptest.NewRecorder()
	writeCallbackPage(rec, "<srv>", errors.New(`<script>alert(1)</script>`))
	if body := rec.Body.String(); strings.Contains(body, "<script>alert") || rec.Code != 400 {
		t.Fatalf("unescaped failure page (%d):\n%s", rec.Code, body)
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
