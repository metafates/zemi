package ui

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
)

// TestGoldenWorkflows pins a run's view at 80 columns, opened by enter on its
// /workflows row, running and then paused: pausing changes the status value
// and the footer's word, and no other cell of the view. The agents talk to a fake provider that answers
// the collect agent, billing its tokens, and holds the reviewers until the
// test ends.
func TestGoldenWorkflows(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "Collect") {
			select {
			case <-hold:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"two files changed"},"finish_reason":null}]}`,
			`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":300,"total_tokens":1500}}`,
			`[DONE]`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
	}))
	defer srv.Close()
	defer close(hold)

	app, host := bootAgentApp(t, 80, 30)
	ctx := t.Context()
	reloadUI(t, host)
	runLua(t, app, fmt.Sprintf(`
		zemi.provider.add({ id = "p", kind = "openai_compat", base_url = %q, api_key = "k", models = { { id = "m" } } })
		zemi.config.set("models.chat", "p/m")
		zemi.workflow.run({ concurrency = 2, script = [[
			-- Review the changes
			local wf = zemi.workflow
			wf.phase("collect")
			local changes = wf.agent("Collect the changes", { agent = "explore", description = "collect changes" })
			wf.phase("review")
			wf.parallel({
				function() return wf.agent("Review for correctness:\n" .. changes, { agent = "explore", description = "correctness" }) end,
				function() return wf.agent("Review for security:\n" .. changes, { agent = "explore", description = "security" }) end,
				function() return wf.agent("Review for simplicity:\n" .. changes, { agent = "explore", description = "simplicity" }) end,
			})
		]] })`, srv.URL+"/v1"))

	// the reviewers stay held for as long as the test runs, so the clock keeps
	// ticking and any duration it prints normalizes to one value, whatever
	// width the seconds grew to
	elapsed, runID := regexp.MustCompile(`\d+(\.\d+)?s`), regexp.MustCompile(`run +([0-9a-hjkmnp-tv-z]{8})`)
	frame := func() string {
		t.Helper()
		for app.Pump() {
		}
		app.renderSync(t)
		out := app.Frame()
		// the ids first: random base32 can hold a "<digit>s" pair that the
		// clock would rewrite, leaving a half-masked id the mask can no
		// longer find, and a frame can hold the run's and an agent's
		if m := runID.FindAllStringSubmatch(ansi.Strip(out), -1); len(m) > 0 {
			for _, id := range m {
				out = strings.ReplaceAll(out, id[1], "0000000a")
			}
		}
		return elapsed.ReplaceAllString(out, "0.0s")
	}
	if err := host.RunCommand(ctx, "workflows", ""); err != nil {
		t.Fatal(err)
	}
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 })
	pressKeys(app, "enter")
	waitPrompt(t, app, func() bool {
		return len(app.overlays) == 2 && strings.Contains(frame(), "2 running")
	})
	// opened again with its agents: a list that opens empty keeps the
	// wider box of a text view
	pressKeys(app, "esc")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 1 })
	pressKeys(app, "enter")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 2 })
	running := frame()
	t.Run("running", func(t *testing.T) { golden.RequireEqual(t, running) })

	// the collect agent's preview: its facts, its task and what it answered,
	// the view enter opens on an agent row of the run and of /agents alike
	pressKeys(app, "enter")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 3 })
	t.Run("agent", func(t *testing.T) { golden.RequireEqual(t, frame()) })
	pressKeys(app, "esc")
	waitPrompt(t, app, func() bool { return len(app.overlays) == 2 })

	pressKeys(app, "p")
	waitPrompt(t, app, func() bool { return strings.Contains(frame(), "paused") })
	app.toasts = nil // the pause's toast sits above the view
	paused := frame()
	t.Run("paused", func(t *testing.T) { golden.RequireEqual(t, paused) })

	a, b := strings.Split(running, "\n"), strings.Split(paused, "\n")
	var moved []string
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			moved = append(moved, ansi.Strip(b[i]))
		}
	}
	if len(a) != len(b) || len(moved) != 2 || !strings.Contains(moved[0], "paused") || !strings.Contains(moved[1], "p resume") {
		t.Fatalf("pausing moved more than the status and the footer word: %q", moved)
	}
}
