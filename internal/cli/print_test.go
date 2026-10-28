package cli

// TestPrintMode: -p boots headless, sends the prompt, prints the final
// reply and exits 0; a turn that ends in an error row exits 1 with the
// reply still printed. The provider is a local OpenAI-compatible server.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
)

func printTestProvider(t *testing.T, reply string, status int) config.Dirs {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		chunk := func(delta, finish string) string {
			b, _ := json.Marshal(map[string]any{
				"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "m1",
				"choices": []map[string]any{{
					"index": 0, "finish_reason": finish,
					"delta": map[string]any{"content": delta},
				}},
			})
			return "data: " + string(b) + "\n\n"
		}
		_, _ = w.Write([]byte(chunk(reply, "") + chunk("", "stop") + "data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	for _, env := range []string{"CONFIG", "DATA", "STATE", "CACHE"} {
		t.Setenv("ZEMI_"+env+"_HOME", filepath.Join(home, strings.ToLower(env)))
	}
	base := t.TempDir()
	t.Chdir(base)
	t.Setenv("PWD", base)
	writeFile(t, filepath.Join(home, "config", "init.lua"),
		`zemi.provider.add({ id = "gw", kind = "openai_compat", api_key = "x", base_url = "`+srv.URL+`/v1", models = { { id = "m1" } } })`)
	dirs, err := config.ResolveDirs()
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func TestPrintMode(t *testing.T) {
	t.Run("the reply prints and the run exits 0", func(t *testing.T) {
		dirs := printTestProvider(t, "printed reply", http.StatusOK)
		var out, errOut strings.Builder
		code := runPrint(options{prompt: "say something", dirs: dirs}, &out, &errOut)
		if code != 0 {
			t.Fatalf("runPrint = %d (stderr %q), want 0", code, errOut.String())
		}
		if got := out.String(); !strings.Contains(got, "printed reply") {
			t.Fatalf("stdout = %q, want the reply", got)
		}
	})
	t.Run("no prompt anywhere is a usage error", func(t *testing.T) {
		dirs := printTestProvider(t, "x", http.StatusOK)
		devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdin
		os.Stdin = devNull
		t.Cleanup(func() { os.Stdin = old })
		var out, errOut strings.Builder
		if code := runPrint(options{dirs: dirs}, &out, &errOut); code != 2 {
			t.Fatalf("runPrint = %d (stderr %q), want 2", code, errOut.String())
		}
	})
}
