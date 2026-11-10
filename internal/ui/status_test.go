package ui

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/anim"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/status"
	"github.com/metafates/zemi/internal/tools"
)

// statusApp is a bare zemi for the program status report's tests.
func statusApp(t *testing.T) (*App, *hostapi.Host) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	host := hostapi.NewHost(lua.NewRuntime(lua.RuntimeOptions{Log: log}), log, config.NewStore(), config.Dirs{})
	t.Cleanup(host.Close)
	return NewApp(host, log), host
}

// TestScrollPillClicksToBottom: clicking the scroll pill does what its key
// does, returning the view to the tail.
func TestScrollPillClicksToBottom(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	for len(host.Session().Transcript) < 40 {
		host.AppendMessage("user", fmt.Sprintf("row %d", len(host.Session().Transcript)+1))
	}
	app.renderSync(t)
	app.applyScroll(hostapi.ScrollTop, _regionChat)
	app.renderSync(t)
	app.Frame()
	var pill *widget
	for _, w := range app.widgets {
		if w.id == "statusline.scroll" {
			pill = w
		}
	}
	if pill == nil {
		t.Fatal("the scroll pill is missing")
	}
	pill.onClick(app, nil)
	app.renderSync(t)
	app.Frame()
	st := app.scrolls[app.scrollDef]
	if st == nil || !st.follow || app.rowsBelow() != 0 {
		t.Fatalf("the pill's click left the view parked: follow %v, rowsBelow %d", st != nil && st.follow, app.rowsBelow())
	}
}

// statusFrame sends one frame and returns the OSC 7501 it carried, "" for none.
func statusFrame(app *App) string {
	_, cmd := app.Update(frameTickMsg{})
	app.wakeTick()
	for _, m := range runCmd(cmd) {
		if raw, ok := m.(tea.RawMsg); ok {
			if s := fmt.Sprint(raw.Msg); strings.Contains(s, "\x1b]7501;") {
				return s
			}
		}
	}
	return ""
}

// b64 encodes text as the protocol carries free-text values.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// report is the exact sequence the app sends for a root-record state.
func report(state status.State, kind status.Kind, msg string) string {
	s := "\x1b]7501;state=" + state.String()
	if kind != 0 {
		s += ":kind=" + kind.String()
	}
	s += ":app=zemi:title=" + b64("zemi")
	if msg != "" {
		s += ":msg=" + b64(msg)
	}
	return s + "\x1b\\"
}

// TestProgramStatus: the record the terminal holds follows the state the
// frame drew — working on the turn's own words, blocked with a kind while an
// ask waits, done or error as the turn ends, idle once the user cancelled —
// and an unchanged state is never resent.
func TestProgramStatus(t *testing.T) {
	t.Parallel()
	app, host := statusApp(t)
	const task = "migrate the config"
	idle := report(status.Idle, 0, "")
	if got := statusFrame(app); got != idle {
		t.Fatalf("boot: sent %q", got)
	}
	if got := statusFrame(app); got != "" {
		t.Fatalf("an unchanged state resent %q", got)
	}

	// a turn runs on the user's words
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Task: task})
	working := report(status.Working, 0, task)
	if got := statusFrame(app); got != working {
		t.Fatalf("working: sent %q", got)
	}

	// a permission ask blocks with what it wants, then the answer returns
	// to the still-running turn
	app.testQueue = make(chan tea.Msg, 1)
	answered := make(chan struct{}, 1)
	go func() {
		app.AskPermission(hostapi.PermRequest{Tool: "shell", Subject: perm.SubjectShell, Arg: "make check", Description: "run the check"})
		answered <- struct{}{}
	}()
	app.Update(<-app.testQueue)
	if got := statusFrame(app); got != report(status.Blocked, status.KindPermission, "Run this command? run the check") {
		t.Fatalf("ask: sent %q", got)
	}
	app.answerPerm(true, "user:once")
	<-answered
	if got := statusFrame(app); got != working {
		t.Fatalf("answered: sent %q", got)
	}

	// the trust ask names the root the way the window spells it
	app.trust = &trustPrompt{root: "/proj"}
	if got := statusFrame(app); got != report(status.Blocked, status.KindPermission, "Trust this project? /proj") {
		t.Fatalf("trust: sent %q", got)
	}
	app.trust = nil

	// a plan review blocks as its own kind: it answers a/s/e/esc, not y|n
	app.planAsk = &planAskPrompt{plan: "1. do it"}
	if got := statusFrame(app); got != report(status.Blocked, status.KindPlan, "A plan to review 1. do it") {
		t.Fatalf("plan: sent %q", got)
	}
	app.planAsk = nil

	// a question blocks with its first line, and a batch says how many wait
	app.question = &questionPrompt{qs: []tools.Question{{Question: "which database?"}}, answer: make(chan qAnswer, 1)}
	if got := statusFrame(app); got != report(status.Blocked, status.KindQuestion, "which database?") {
		t.Fatalf("question: sent %q", got)
	}
	app.question = &questionPrompt{qs: []tools.Question{{Question: "which database?"}, {Question: "which driver?"}}, answer: make(chan qAnswer, 1)}
	if got := statusFrame(app); got != report(status.Blocked, status.KindQuestion, "which database? (1 of 2)") {
		t.Fatalf("question batch: sent %q", got)
	}
	app.finishQuestion(false)

	// the turn ends: done carries the reply it left, error its one-line
	// cause, and a user's interrupt reports idle
	host.AppendChat(hostapi.ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "migrated, tests green"})
	host.SeedStream(hostapi.StreamSnapshot{Task: task})
	app.Update(turnDoneMsg{})
	if got := statusFrame(app); got != report(status.Done, 0, "migrated, tests green") {
		t.Fatalf("done: sent %q", got)
	}
	// a turn that only ran tools says nothing of itself
	host.AppendChat(hostapi.ChatMessage{Role: fantasy.MessageRoleUser, Text: task})
	host.SeedStream(hostapi.StreamSnapshot{Task: task})
	app.Update(turnDoneMsg{})
	if got := statusFrame(app); got != report(status.Done, 0, "") {
		t.Fatalf("a wordless turn: sent %q", got)
	}
	host.SeedStream(hostapi.StreamSnapshot{Task: task})
	app.Update(turnDoneMsg{err: "provider refused\n\tand again"})
	if got := statusFrame(app); got != report(status.Error, 0, "provider refused and again") {
		t.Fatalf("error: sent %q", got)
	}
	host.SeedStream(hostapi.StreamSnapshot{})
	app.Update(turnDoneMsg{stopped: true})
	if got := statusFrame(app); got != idle {
		t.Fatalf("interrupted: sent %q", got)
	}
}

// TestPlanStatus: the record and the alert name the plan's opening line,
// its heading marker off, and fall back to the bare label without one.
func TestPlanStatus(t *testing.T) {
	for _, c := range []struct{ plan, want string }{
		{"add retry to the deploy step", "A plan to review  add retry to the deploy step"},
		{"# Migration plan\nthen move the config", "A plan to review  Migration plan"},
		{"```go\npackage main", "A plan to review"},
		{"  \nsecond line first", "A plan to review"},
		{strings.Repeat("x", 300), "A plan to review  " + strings.Repeat("x", 199) + "…"},
	} {
		if got := planStatus(c.plan); got != c.want {
			t.Errorf("planStatus(%q) = %q, want %q", c.plan, got, c.want)
		}
	}
}

// TestStatusRise: a statusline pill rises out of the surface when it starts
// drawing and sinks back in when it stops, each over _riseDuration on its
// own clock behind the boot's delay, and the ramps keep the tick awake while
// they run.
func TestStatusRise(t *testing.T) {
	_, host := statusApp(t)
	app := &App{
		host:    host,
		widgets: statuslineWidgets(),
		rises:   map[string]*anim.Curtain{},
		caches:  map[string]*widgetCache{},
		now:     func() time.Time { return time.UnixMilli(500) },
	}
	const id = "statusline.goal"
	lines := [][]hostapi.Segment{{{Text: "flag", HL: "Dim"}}}

	// appear: the first draw starts the rise
	app.onWidgetRendered(widgetRender{id: id, lines: lines, gen: 1, seq: 1, width: 80, height: 14})
	if got := app.caches[id].lines[0][0].Fade; got != 1 {
		t.Fatalf("the pill's first draw drew at fade %v, want fully sunk", got)
	}
	if !app.fadesRising() || app.tickInterval() != _riseTick {
		t.Fatal("the rise did not keep the tick awake")
	}
	app.now = func() time.Time {
		return time.UnixMilli(500 + _barDelay.Milliseconds() + _riseDuration.Milliseconds()/2)
	}
	app.onWidgetRendered(widgetRender{id: id, lines: lines, gen: 2, seq: 2, width: 80, height: 14})
	if got := app.caches[id].lines[0][0].Fade; got < 0.4 || got > 0.6 {
		t.Fatalf("mid-rise fade %v, want about half", got)
	}
	app.now = func() time.Time { return time.UnixMilli(1500) }
	app.onWidgetRendered(widgetRender{id: id, lines: lines, gen: 3, seq: 3, width: 80, height: 14})
	if got := app.caches[id].lines[0][0].Fade; got != 0 {
		t.Fatalf("the settled pill kept a fade of %v", got)
	}
	if app.fadesRising() || app.tickInterval() != _idleInterval {
		t.Fatal("the finished rise kept the tick awake")
	}

	// disappear: an empty draw keeps the last lines, sinking back in from
	// full brightness
	app.now = func() time.Time { return time.UnixMilli(2000) }
	app.onWidgetRendered(widgetRender{id: id, gen: 4, seq: 4, width: 80, height: 14})
	if got := app.caches[id].lines[0][0].Fade; got != 0 {
		t.Fatalf("the vanishing pill drew at fade %v, want fully bright", got)
	}
	app.now = func() time.Time { return time.UnixMilli(2000 + _riseDuration.Milliseconds()/2) }
	app.onWidgetRendered(widgetRender{id: id, gen: 5, seq: 5, width: 80, height: 14})
	if got := app.caches[id].lines[0][0].Fade; got < 0.4 || got > 0.6 {
		t.Fatalf("mid-fall fade %v, want about half", got)
	}
	if !app.fadesRising() {
		t.Fatal("the fall did not keep the tick awake")
	}
	app.now = func() time.Time { return time.UnixMilli(3000) }
	app.onWidgetRendered(widgetRender{id: id, gen: 6, seq: 6, width: 80, height: 14})
	if len(app.caches[id].lines) != 0 {
		t.Fatalf("the fallen pill kept %d lines", len(app.caches[id].lines))
	}
	if app.fadesRising() || app.tickInterval() != _idleInterval {
		t.Fatal("the finished fall kept the tick awake")
	}
}

// TestCostPlacement: the cost pill sits outside the ctx pill while keeping
// its priority, so a short row still sheds the cost before the ctx count.
func TestCostPlacement(t *testing.T) {
	t.Parallel()
	app := &App{
		widgets: statuslineWidgets(),
		rises:   map[string]*anim.Curtain{},
		caches:  map[string]*widgetCache{},
		now:     func() time.Time { return time.UnixMilli(500) },
	}
	for id, text := range map[string]string{"statusline.context": "1k/2k ctx", "statusline.cost": "$0.12"} {
		app.onWidgetRendered(widgetRender{id: id, lines: [][]hostapi.Segment{{{Text: text}}}, gen: 1, seq: 1, width: 80, height: 14})
	}
	items := app.statusItems(_regionStatusRight)
	at := func(id string) int {
		for i, it := range items {
			if hostapi.SegsText(it.segs) == map[string]string{"statusline.context": "1k/2k ctx", "statusline.cost": "$0.12"}[id] {
				return i
			}
		}
		t.Fatalf("%s draws no item", id)
		return 0
	}
	// items run outermost first, so the cost sits outside the ctx count
	if at("statusline.cost") > at("statusline.context") {
		t.Fatalf("the ctx pill sits outside the cost: %v", items)
	}
	for _, it := range items {
		if text := hostapi.SegsText(it.segs); text == "$0.12" && it.pri != 42 {
			t.Fatalf("the cost lost its drop priority %v", it.pri)
		}
	}
}

// TestContextDip: the ctx pill dips to a new count — the changed digit
// dissolves (the old one sinks, the new one rises once it lands) while the
// rest of the pill stands — and a re-render that says the same words does
// not dip.
func TestContextDip(t *testing.T) {
	_, host := statusApp(t)
	app := &App{
		host:    host,
		widgets: statuslineWidgets(),
		rises:   map[string]*anim.Curtain{},
		caches:  map[string]*widgetCache{},
		now:     func() time.Time { return time.UnixMilli(500) },
	}
	const id = "statusline.context"
	first := [][]hostapi.Segment{{{Text: "1k/2k ctx", HL: "Dim"}}}
	draw := func(gen int64, lines [][]hostapi.Segment, at time.Time) hostapi.Segment {
		app.now = func() time.Time { return at }
		app.onWidgetRendered(widgetRender{id: id, lines: lines, gen: gen, seq: gen, width: 80, height: 14})
		return app.caches[id].lines[0][0]
	}
	up := time.UnixMilli(500).Add(_barDelay + _riseDuration)

	if got := draw(1, first, time.UnixMilli(500)); got.Fade != 1 {
		t.Fatalf("the pill's first draw drew at fade %v, want fully sunk", got.Fade)
	}
	if got := draw(2, first, up); got.Text != "1k/2k ctx" || got.Fade != 0 {
		t.Fatalf("the risen pill did not draw clean: %+v", got)
	}
	if got := draw(3, first, up); got.Text != "1k/2k ctx" || got.Fade != 0 {
		t.Fatalf("an unchanged pill dipped: %+v", got)
	}

	second := [][]hostapi.Segment{{{Text: "2k/2k ctx", HL: "Dim"}}}
	if got := draw(4, second, up); got.Text != "1k/2k ctx" || got.Fade != 0 {
		t.Fatalf("the dip did not sink the old count whole: %+v", got)
	}
	if !app.fadesRising() || app.tickInterval() != _riseTick {
		t.Fatal("the dip did not keep the tick awake")
	}
	if got := draw(5, second, up.Add(_dipDuration/2)); got.Text != "1" || got.Fade < 0.4 || got.Fade > 0.6 {
		t.Fatalf("the dip did not sink the changed digit alone: %+v", got)
	}
	if row := app.caches[id].lines[0]; len(row) != 2 || row[1].Text != "k/2k ctx" || row[1].Fade != 0 {
		t.Fatalf("the dip did not leave the shared text standing: %+v", row)
	}
	if got := draw(6, second, up.Add(_dipDuration)); got.Text != "2" || got.Fade != 1 {
		t.Fatalf("the dip did not hand over to the new count: %+v", got)
	}
	third := [][]hostapi.Segment{{{Text: "3k/2k ctx", HL: "Dim"}}}
	if got := draw(7, third, up.Add(_dipDuration+_dipDuration/2)); got.Text != "3" || got.Fade >= 1 {
		t.Fatalf("a count mid-rise did not ride the dip out: %+v", got)
	}
	if app.rises[id].Sinking() {
		t.Fatal("a count mid-rise re-dipped the pill")
	}
	if got := draw(8, third, up.Add(2*_dipDuration)); got.Fade != 0 {
		t.Fatalf("the dipped pill kept a fade of %v", got.Fade)
	}
}

// TestModelDipWhole: a model change sinks the model pill and the ctx pill
// whole at the appear's speed — a name sharing a prefix with the old one
// fades with it, and the ctx pill rides the change with its count's words
// unchanged — where a plain dip keeps what the words share.
func TestModelDipWhole(t *testing.T) {
	_, host := statusApp(t)
	app := &App{
		host:    host,
		widgets: statuslineWidgets(),
		rises:   map[string]*anim.Curtain{},
		caches:  map[string]*widgetCache{},
		now:     func() time.Time { return time.UnixMilli(500) },
	}
	model, ctx := "statusline.model", "statusline.context"
	draw := func(gen int64, name, count string, at time.Time) {
		app.now = func() time.Time { return at }
		for _, w := range []struct{ id, text string }{{model, name}, {ctx, count + " ctx"}} {
			app.onWidgetRendered(widgetRender{
				id: w.id, lines: [][]hostapi.Segment{{{Text: w.text, HL: "Dim"}}},
				gen: gen, seq: gen, width: 80, height: 14, model: name,
			})
		}
	}
	up := time.UnixMilli(500).Add(_barDelay + _riseDuration)

	draw(1, "AgentLLM-big", "1k/2k", time.UnixMilli(500))
	if got := app.caches[model].lines[0][0]; got.Fade != 1 {
		t.Fatalf("the pill's first draw drew at fade %v, want fully sunk", got.Fade)
	}
	draw(2, "AgentLLM-big", "1k/2k", up)
	if got := app.caches[model].lines[0][0]; got.Text != "AgentLLM-big" || got.Fade != 0 {
		t.Fatalf("the risen pill did not draw clean: %+v", got)
	}
	// the model change sinks both pills whole, the ctx count's words unchanged
	draw(3, "AgentLLM-small", "1k/2k", up)
	for id, want := range map[string]string{model: "AgentLLM-big", ctx: "1k/2k ctx"} {
		if got := app.caches[id].lines[0][0]; got.Text != want || got.Fade != 0 {
			t.Fatalf("the sink did not take %s's old words whole: %+v", id, got)
		}
	}
	draw(4, "AgentLLM-small", "1k/2k", up.Add(_riseDuration/2))
	for _, id := range []string{model, ctx} {
		if row := app.caches[id].lines[0]; len(row) != 1 || row[0].Fade < 0.4 || row[0].Fade > 0.6 {
			t.Fatalf("%s did not sink whole mid-dip: %+v", id, row)
		}
	}
	draw(5, "AgentLLM-small", "1k/2k", up.Add(_riseDuration))
	if got := app.caches[model].lines[0][0]; got.Text != "AgentLLM-small" || got.Fade != 1 {
		t.Fatalf("the dip did not hand over to the new name: %+v", got)
	}
}
