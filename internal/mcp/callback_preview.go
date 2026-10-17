//go:build ignore

// callback_preview.go renders the sign-in callback page (callback.html) in
// both states for eyeballing, without a real provider: make mcp-auth.
package main

import (
	"html/template"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
)

// the first port listenCallback tries, so the URL matches a real redirect
const port = "40704"

func main() {
	_, this, _, _ := runtime.Caller(0)
	page := filepath.Join(filepath.Dir(this), "callback.html")
	render := func(w http.ResponseWriter, ok bool, server, reason string) {
		// parsed per request: editing callback.html shows up on refresh
		tpl, err := template.ParseFiles(page)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
		}
		_ = tpl.Execute(w, struct {
			Server, Reason string
			OK             bool
		}{server, reason, ok})
	}
	http.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		render(w, true, "https://mcp.example.com/mcp", "")
	})
	http.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		reason := r.URL.Query().Get("reason")
		if reason == "" {
			reason = "token exchange failed: http 400 invalid_grant: the authorization code expired before the exchange"
		}
		render(w, false, "", reason)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/callback", http.StatusFound)
	})
	ok, fail := "http://localhost:"+port+"/callback", "http://localhost:"+port+"/fail"
	ln, err := net.Listen("tcp", "localhost:"+port)
	if err != nil {
		println("already serving, opening the browser")
		_ = exec.Command("open", ok, fail).Start()
		return
	}
	println("serving " + ok + " and " + fail)
	if err := exec.Command("open", ok, fail).Start(); err != nil {
		println("open a browser at " + ok + " and " + fail)
	}
	if err := http.Serve(ln, nil); err != nil {
		panic(err)
	}
}
