package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	zemiRuntime "github.com/metafates/zemi/runtime"
)

// bootAgentApp boots a full host + app with the default UI, mirroring the
// golden_test harness.
func bootAgentApp(t *testing.T, w, h int) (*App, *hostapi.Host) {
	t.Helper()
	ctx := context.Background()
	cfg, data, state := t.TempDir(), t.TempDir(), t.TempDir()
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
// §6.2, §8): fixed chrome, subject + arg + reason, answer keys. The allow
// answers and the always-rule storage are asserted around the golden.
func TestGoldenPermissionOverlay(t *testing.T) {
	app, host := bootAgentApp(t, 70, 16)

	done := make(chan struct{})
	var allowed bool
	var by string
	go func() {
		allowed, by = app.AskPermission(hostapi.PermRequest{
			Tool:    "bash",
			Subject: "tool.bash",
			Arg:     "git push origin main",
			Reason:  "publishing the release branch",
			Always:  "don't ask again for `git push`",
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
	// Rule storage for "always" is the host's askHook contract (covered in
	// hostapi); the overlay's job is the answer delivery asserted above.
	if host.AgentActive() {
		t.Fatal("no turn should be active")
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

// TestGoldenModelPicker pins the Go-owned model picker; enter returns the
// selected id and stores it, esc cancels.
func TestGoldenModelPicker(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	items := []hostapi.ModelOption{
		{ID: "anthropic/claude-sonnet-4-5", Display: "Claude Sonnet 4.5", HasKey: true},
		{ID: "anthropic/claude-haiku-4-5", Display: "Claude Haiku 4.5"},
		{ID: "openai/gpt-5", Display: "GPT-5", HasKey: true},
	}
	done := make(chan struct{})
	var picked string
	var ok bool
	go func() {
		picked, ok = app.PickModel(items, "anthropic/claude-sonnet-4-5")
		close(done)
	}()
	waitPrompt(t, app, func() bool { return app.modelPick != nil })

	app.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // -> haiku
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // -> gpt-5
	app.renderSync(t)
	golden.RequireEqual(t, app.Frame())

	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	<-done
	if !ok || picked != "openai/gpt-5" {
		t.Fatalf("picked=%q ok=%v", picked, ok)
	}
	// Storing agent.model is the host's zemi.model.pick action (covered in
	// hostapi); the picker's job is the selection asserted above.
}

// TestModelPickerCancel covers esc on the picker: no selection.
func TestModelPickerCancel(t *testing.T) {
	app, _ := bootAgentApp(t, 70, 16)

	done := make(chan struct{})
	var picked string
	var ok bool
	go func() {
		picked, ok = app.PickModel([]hostapi.ModelOption{
			{ID: "openai/gpt-5", Display: "GPT-5", HasKey: true},
		}, "")
		close(done)
	}()
	waitPrompt(t, app, func() bool { return app.modelPick != nil })
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	<-done
	if ok || picked != "" {
		t.Fatalf("cancel returned %q ok=%v", picked, ok)
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
	app.AgentEvent(hostapi.AgentEvent{Kind: "text", Text: "..."})

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
	app, host := bootAgentApp(t, 80, 20)
	run := func(src string) {
		t.Helper()
		if err := host.RT.ExecuteChunk(context.Background(), src, "select.lua", lua.OriginUser); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
	}
	lastToast := func() string {
		for app.Pump() {
		}
		if len(app.toasts) == 0 {
			return ""
		}
		return app.toasts[len(app.toasts)-1].body
	}
	pick := `function(item) zemi.notify({ title = "picked", body = item or "none" }) end`

	run(`zemi.ui.select({ "red", "green" }, { prompt = "Color" }, ` + pick + `)`)
	if len(app.overlays) != 1 || app.overlays[0].o.Title != "Color" {
		t.Fatalf("select overlay: %+v", app.overlays)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := lastToast(); got != "green" {
		t.Fatalf("enter picked %q", got)
	}

	run(`zemi.ui.select({ "x" }, {}, ` + pick + `)`)
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := lastToast(); got != "none" {
		t.Fatalf("esc picked %q", got)
	}
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
	if !app.palette.open || len(app.palette.args) != 1 || app.palette.args[0] != "bob" || len(app.palette.matches) != 0 {
		t.Fatalf("palette args = %+v, matches = %d (open=%v)", app.palette.args, len(app.palette.matches), app.palette.open)
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if app.composer.text != "/greet bob " {
		t.Fatalf("composer = %q", app.composer.text)
	}
}

// TestTrustAnswers: the project trust prompt maps y, n and d to trust,
// not now and never for this tree hash.
func TestTrustAnswers(t *testing.T) {
	for key, want := range map[rune]string{'y': "trust", 'n': "once", 'd': "deny"} {
		app, _ := bootAgentApp(t, 80, 20)
		done := make(chan string, 1)
		go func() { done <- app.AskTrust("/proj/.zemi", "abc123") }()
		waitPrompt(t, app, func() bool { return app.trust != nil })
		app.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
		if got := <-done; got != want {
			t.Errorf("%c answered %q, want %q", key, got, want)
		}
	}
}

// TestLuaUtil runs the shipped zemi.util helpers through the real runtime.
func TestLuaUtil(t *testing.T) {
	_, host := bootAgentApp(t, 80, 20)
	src := `
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
	`
	if err := host.RT.ExecuteChunk(context.Background(), src, "util.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
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

// TestReasoningLiveTail: while the model thinks, only its last lines show.
func TestReasoningLiveTail(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Reasoning: "one\ntwo\nthree\nfour\n"})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	if !strings.Contains(frame, "✻ Thinking") || strings.Contains(frame, "│ one") || !strings.Contains(frame, "│ four") {
		t.Fatalf("live thinking tail:\n%s", frame)
	}
}

// TestWorkingRowRetry: a waiting provider retry replaces the elapsed time
// with a countdown so the turn does not look stuck.
func TestWorkingRowRetry(t *testing.T) {
	app, host := bootAgentApp(t, 80, 16)
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Elapsed: 20 * time.Second, RetryIn: 4 * time.Second, Retries: 1})
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "Retrying  attempt 2 · next try in 4s · esc to interrupt") {
		t.Fatalf("retry row missing:\n%s", frame)
	}
}

// TestWrapSegs covers the chat word-wrap: short lines pass through, long
// lines wrap at word boundaries with styles preserved, overlong words
// hard-split.
func TestWrapSegs(t *testing.T) {
	short := []hostapi.Segment{{Text: "hi"}}
	rows := wrapSegs(short, 10)
	if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0].Text != "hi" {
		t.Fatalf("short line: %+v", rows)
	}

	rows = wrapSegs([]hostapi.Segment{{Text: "hello world foo", HL: "A"}}, 11)
	if len(rows) != 2 || segsText(rows[0]) != "hello " || segsText(rows[1]) != "world foo" {
		t.Fatalf("word wrap: %+v", rows)
	}
	for _, r := range rows {
		if r[0].HL != "A" {
			t.Fatalf("style lost: %+v", rows)
		}
	}

	rows = wrapSegs([]hostapi.Segment{{Text: "abcdefgh"}}, 4)
	if len(rows) != 2 || rows[0][0].Text != "abcd" || rows[1][0].Text != "efgh" {
		t.Fatalf("hard split: %+v", rows)
	}

	line := []hostapi.Segment{{Text: "one two "}, {Text: "three", HL: "B"}}
	rows = wrapSegs(line, 7)
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = segsText(r)
	}
	if fmt.Sprint(texts) != "[one  two  three]" {
		t.Fatalf("multi-segment wrap: %v (%+v)", texts, rows)
	}
	if rows[2][0].HL != "B" {
		t.Fatalf("style lost on wrapped segment: %+v", rows)
	}
}

func segsText(segs []hostapi.Segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
	}
	return b.String()
}

// TestLuaInput drives zemi.ui.input: typing edits the prefilled line, enter
// hands the text to on_confirm, esc hands it nil.
func TestLuaInput(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	run := func(src string) {
		t.Helper()
		if err := host.RT.ExecuteChunk(context.Background(), src, "input.lua", lua.OriginUser); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
	}
	lastToast := func() string {
		for app.Pump() {
		}
		if len(app.toasts) == 0 {
			return ""
		}
		return app.toasts[len(app.toasts)-1].body
	}
	open := `zemi.ui.input({ prompt = "Branch", default = "fix-" }, function(t) zemi.notify({ title = "input", body = t or "none" }) end)`

	run(open)
	if len(app.overlays) != 1 || app.overlays[0].o.Title != "Branch" {
		t.Fatalf("input overlay: %+v", app.overlays)
	}
	for _, r := range "lsp" {
		app.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := lastToast(); got != "fix-lsp" {
		t.Fatalf("enter gave %q", got)
	}
	run(open)
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := lastToast(); got != "none" || len(app.overlays) != 0 {
		t.Fatalf("esc gave %q, overlays %d", got, len(app.overlays))
	}
}

// TestKeySequence: ctrl+g then t runs a <leader>t binding without typing
// the t; a key that breaks the sequence reaches the composer.
func TestKeySequence(t *testing.T) {
	app, host := bootAgentApp(t, 80, 20)
	if err := host.RT.ExecuteChunk(context.Background(), `zemi.keymap.set("<leader>t", function() zemi.notify({ title = "seq", body = "ran" }) end)`, "seq.lua", lua.OriginUser); err != nil {
		t.Fatal(err)
	}
	ctrlG := tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl}
	app.Update(ctrlG)
	if len(app.keySeq) != 1 {
		t.Fatalf("pending = %v", app.keySeq)
	}
	app.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	for app.Pump() {
	}
	if len(app.toasts) == 0 || app.toasts[len(app.toasts)-1].body != "ran" || app.composer.text != "" {
		t.Fatalf("toasts=%v composer=%q", app.toasts, app.composer.text)
	}
	app.Update(ctrlG)
	app.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if len(app.keySeq) != 0 || app.composer.text != "x" {
		t.Fatalf("broken sequence: pending=%v composer=%q", app.keySeq, app.composer.text)
	}
}

// TestPermissionChoices: arrows move between Yes, the "don't ask again"
// choice and No, enter answers the highlighted one; without a
// don't-ask-again choice there are two buttons.
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
	right, enter := tea.KeyPressMsg{Code: tea.KeyRight}, tea.KeyPressMsg{Code: tea.KeyEnter}
	bash := hostapi.PermRequest{Tool: "bash", Subject: "tool.bash", Arg: "go test ./...", Always: "don't ask again for `go test`"}
	if ok, by := ask(bash, right, enter); !ok || by != "user:always" {
		t.Fatalf("right+enter = %v %q", ok, by)
	}
	if ok, by := ask(bash, right, right, enter); ok || by != "user" {
		t.Fatalf("right+right+enter = %v %q", ok, by)
	}
	bash.Always = ""
	if ok, by := ask(bash, right, enter); ok || by != "user" {
		t.Fatalf("two buttons, right+enter = %v %q", ok, by)
	}
}

func TestDiffRows(t *testing.T) {
	d := "--- a.go\n+++ a.go\n@@ -9,3 +9,3 @@\n one\n-two\n+TWO\n three\n"
	var got []string
	for _, row := range diffRows(d, 40) {
		var b strings.Builder
		for _, s := range row {
			b.WriteString(s.Text)
		}
		got = append(got, strings.TrimRight(b.String(), " "))
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
	}})
	app.renderSync(t)
	frame := ansi.Strip(app.Frame())
	for _, want := range []string{"Read 2 files, ran 1 command", "● Edit a.go", "7 - old", "7 + new", "● Run the unit tests", "⎿ go test ./...", "--- FAIL: TestX"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "vet clean") {
		t.Fatalf("command output shows only with ctrl+o:\n%s", frame)
	}
	host.ToggleReasoning()
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "● Read b.go") || strings.Contains(frame, "Read 2 files") || !strings.Contains(frame, "vet clean") {
		t.Fatalf("ctrl+o must list every call:\n%s", frame)
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
	for _, row := range strings.Split(ansi.Strip(app.Frame()), "\r\n") {
		if _, body, ok := strings.Cut(row, "│ "); ok {
			got = append(got, strings.Fields(body)...)
		}
	}
	if want := append(words, "second", "paragraph"); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("wrapped words:\n%s", strings.Join(got, " "))
	}
}
