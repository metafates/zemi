package ui

import (
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/anim"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/store"
)

// fillChat appends 30 user/reply pairs to the transcript.
func fillChat(host *hostapi.Host) {
	for i := 1; i <= 30; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
		appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500, Model: "anthropic/claude-sonnet-4-5"})
	}
}

// press runs one keypress through the router and pumps the hook queue
// (host actions like zemi.ui.scroll.* dispatch their UI mutation). The
// queue is drained first: earlier host activity (message appends) may
// already have filled it.
func press(t *testing.T, app *App, key tea.KeyPressMsg) {
	t.Helper()
	for app.Pump() {
	}
	app.Update(key)
	for app.Pump() {
	}
}

// TestChatScrollKeys drives the default scroll bindings end to end:
// ctrl+u/pgdown move through the transcript (up half a view, down a page),
// ctrl+home/ctrl+b jump to the ends, and reaching the bottom re-arms tail
// follow.
func TestChatScrollKeys(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()

	if !app.scrolls[_regionChat].follow {
		t.Fatalf("expected tail follow")
	}
	if !strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("tail view should show the latest reply")
	}

	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	if app.scrolls[_regionChat].follow {
		t.Fatalf("ctrl+u should leave tail follow")
	}
	if strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("scrolled view should not show the tail")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyPgDown})
	if !app.scrolls[_regionChat].follow {
		t.Fatalf("pgdown into the bottom should re-arm follow")
	}
	if !strings.Contains(app.Frame(), "reply 30") {
		t.Fatalf("follow should show the tail again")
	}

	// the pair a list moves half a page with: up leaves the tail by half a
	// view and down comes back to it
	ctrlU, ctrlD := tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}, tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl}
	press(t, app, ctrlU)
	st := app.scrolls[_regionChat]
	if half := max(st.viewH/2, 1); st.follow || st.top != st.rows-st.viewH-half {
		t.Fatalf("ctrl+u left the view at %d of %d rows, want half a view up", st.top, st.rows)
	}
	press(t, app, ctrlD)
	if !st.follow {
		t.Fatalf("ctrl+d should come back to the tail")
	}
	// the pair is the chat's whatever the draft holds: a drafted line keeps
	// its text and the view still moves
	app.composer.text, app.composer.cursor = "one\ntwo", len("one\ntwo")
	press(t, app, ctrlU)
	if app.composer.text != "one\ntwo" || st.follow {
		t.Fatalf("ctrl+u over a drafted line: %q, follow %v", app.composer.text, st.follow)
	}
	app.composer.text, app.composer.cursor = "", 0
	press(t, app, ctrlD)
	if !st.follow {
		t.Fatalf("ctrl+d should come back to the tail")
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	if app.scrolls[_regionChat].follow || app.scrolls[_regionChat].top != 0 {
		t.Fatalf("ctrl+home should pin the first row")
	}
	if !strings.Contains(app.Frame(), "message 1") {
		t.Fatalf("top view should show the first message")
	}

	press(t, app, tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	if !app.scrolls[_regionChat].follow {
		t.Fatalf("ctrl+b should re-arm follow")
	}
}

// TestChatScrollShrinks: a scrolled view whose rows shrink by more than its
// offset (a fill restarting, a collapsed block) guesses a row above the
// first line and must land on the content, not panic.
func TestChatScrollShrinks(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	st := app.scrolls[_regionChat]
	st.top, st.rows = 2, st.rows+400 // the last frame drew 400 rows more
	app.frameGen++
	app.renderSync(t)
	if f := app.Frame(); !strings.Contains(f, "message") {
		t.Fatalf("the shrunk transcript is not drawn:\n%s", f)
	}
}

// TestAgentViewSendSnapsToTail: sending from a run that is over shows the
// session at its tail, where the message and its reply land, not at the
// place the session was left at.
func TestAgentViewSendSnapsToTail(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	app.Frame()

	db := testStore(t, host)
	id := store.ShortID()
	if err := db.CreateSession(id, store.ShortID(), "run", "explore"); err != nil {
		t.Fatal(err)
	}
	addRunMessage(t, db, id, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.TextPart{Text: "it reads fine"},
	}})
	if err := host.ViewAgent(id); err != nil {
		t.Fatal(err)
	}
	app.renderSync(t)
	app.Frame()
	app.sendMessage("one more")
	app.renderSync(t)
	app.Frame()
	if st := app.scrolls[app.scrollDef]; !st.follow {
		t.Fatalf("the send left the session off the tail (top %d)", st.top)
	}
}

// TestChatScrollFollows pins the snap-back: sending a message, or a new
// transcript (a reset's wipe), returns the view to the tail.
func TestChatScrollFollows(t *testing.T) {
	for name, act := range map[string]func(*App, *hostapi.Host) error{
		"submit": func(app *App, _ *hostapi.Host) error {
			app.composer.text = "next question"
			app.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return nil
		},
		"reset": func(_ *App, host *hostapi.Host) error {
			// a reset replaces the window, which re-opens at the tail
			host.SetTranscript(nil)
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			app, host := bootAgentApp(t, 80, 14)
			fillChat(host)
			app.renderSync(t)
			app.Frame()
			press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
			if app.scrolls[_regionChat].follow {
				t.Fatalf("precondition: scrolled off the tail")
			}
			if err := act(app, host); err != nil {
				t.Fatal(err)
			}
			for app.Pump() {
			}
			if !app.scrolls[_regionChat].follow {
				t.Fatalf("should snap back to the tail")
			}
		})
	}
}

// TestChatScrollAnchorsThroughFill: a long chat fills its latest rows first,
// and a reader who scrolled up in them keeps their place when the older rows
// land above, though every reply repeats the same lines.
func TestChatScrollAnchorsThroughFill(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	for i := 1; i <= 30; i++ {
		host.AppendMessage("user", fmt.Sprintf("message %d", i))
		appendReply(host, "line a\n\nline b\n\nline c\n\nline d\n\nline e", hostapi.TurnStat{Out: 100, ElapsedMS: 1500})
	}
	app.renderSync(t)
	app.Frame()
	var chat shownWidget
	for _, w := range app.shownWidgets() {
		if w.Region == app.scrolls[_regionChat].region {
			chat = w
		}
	}
	full := app.caches[chat.ID].lines
	fill := func(lines [][]hostapi.Segment, more bool) {
		app.onWidgetRendered(widgetRender{id: chat.ID, lines: lines, more: more, width: chat.width, height: chat.height, gen: app.frameGen})
		app.Frame()
	}
	// the text on screen: the bar's thumb shrinks as the rows land
	text := func() string { return strings.ReplaceAll(ansi.Strip(app.Frame()), "█", "│") }
	// the first pass draws the last three turns
	first := slices.IndexFunc(full, func(l []hostapi.Segment) bool { return strings.Contains(hostapi.SegsText(l), "message 28") })
	fill(full[first:], true)
	press(t, app, tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl})
	want, below := text(), app.rowsBelow()
	fill(full, false)
	if got := text(); got != want || app.rowsBelow() != below {
		t.Fatalf("the fill moved the view (%d rows below, want %d):\n%s\nwant:\n%s", app.rowsBelow(), below, got, want)
	}
}

// TestChatStickyPrompt: while a turn's rows fill the view, its prompt's
// first row stands in for the chat's top row, cut with an ellipsis when the
// prompt has more rows and copied once; it lights like a header, copies
// nothing, and a click or alt+up scrolls back to the prompt. A report's
// notice heads its turn the same way.
func TestChatStickyPrompt(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	const prompt = "fix the flaky TestResume in internal/store: it fails about one run in twenty on the build machine"
	host.AppendMessage("user", prompt)
	for i := range 20 {
		appendReply(host, fmt.Sprintf("step %d", i), hostapi.TurnStat{})
	}
	app.renderSync(t)
	app.Frame()
	st := app.scrolls[_regionChat]
	y := st.rect.Min.Y
	row := func() string {
		return strings.TrimRight(hostapi.SegsText(app.lastRows[y][:len(app.lastRows[y])-1]), " ")
	}
	if !st.follow || !strings.HasPrefix(row(), "▌ fix the flaky") || !strings.HasSuffix(row(), "…") {
		t.Fatalf("following, the top row is %q", row())
	}
	// the process-wide malloc count AllocsPerRun reads moves with the app's
	// other goroutines, so the copy's identity says it was not made again
	first := &st.pins[0][0]
	if st.pinRows(st.lines, st.rect.Dx()); &st.pins[0][0] != first {
		t.Fatal("an unchanged pin copies its row again")
	}
	// a toast over the pin takes its own cells and cuts the pin's short
	app.pushToast(slog.LevelInfo, "saved", "")
	app.Frame()
	if _, _, onToast := app.target(70, y); onToast {
		t.Fatal("a toast over the pin acts as the pin")
	}
	if _, _, beside := app.target(20, y); !beside {
		t.Fatal("the pin beside a toast does not act")
	}
	app.toasts = nil

	if dragTo(app, 3, y, 20, y); app.selectionText() != "" {
		t.Fatalf("the pin copied %q", app.selectionText())
	}
	app.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	app.Update(tea.MouseMotionMsg{X: 5, Y: y})
	app.Frame()
	if over := app.lastRows[y][1].Over; over != "Hover" {
		t.Fatalf("hovered pin drew %q over its text", over)
	}

	// alt+up and a click on the pin both land on the prompt; alt+down with no
	// prompt below returns to the tail
	for _, jump := range []func(){
		func() { press(t, app, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModAlt}) },
		func() { clickAt(app, 5, y) },
	} {
		jump()
		frame := ansi.Strip(app.Frame())
		if st.follow || !headerAt(st.lines, st.top) || len(st.pins) != 0 || strings.Count(frame, "fix the flaky") != 1 {
			t.Fatalf("the jump left top %d (follow %v):\n%s", st.top, st.follow, frame)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModAlt})
		if app.Frame(); !st.follow {
			t.Fatalf("alt+down left top %d short of the tail", st.top)
		}
	}

	// a report's notice pins nothing: only a prompt does
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: hostapi.RowNotice, Text: "Background agent a1 (explore) finished"})
	for i := range 20 {
		appendReply(host, fmt.Sprintf("answer %d", i), hostapi.TurnStat{})
	}
	app.renderSync(t)
	if app.Frame(); strings.Contains(row(), "Background agent a1") {
		t.Fatalf("after a report, the top row is %q", row())
	}
}

// TestThinkingFade: the live thinking tail sinks at its top while it hides
// lines above, over the chat's own four-row ramp and easing off as the hidden
// lines come back; a tail that fits keeps its colors, the rail bar never
// sinks, and a widget's deeper sink outlives the region's.
func TestThinkingFade(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 24)
	host.AppendMessage("user", "look around")
	tail := func(reasoning string) []float64 {
		host.SeedStream(hostapi.StreamSnapshot{Active: true, Reasoning: reasoning, Thinking: true})
		app.renderSync(t)
		app.Frame()
		st := app.scrolls[_regionChat]
		head := slices.IndexFunc(st.lines, func(r []hostapi.Segment) bool {
			return strings.HasSuffix(strings.TrimSpace(hostapi.SegsText(r)), "thinking")
		})
		if head < 0 {
			t.Fatal("no thinking row")
		}
		var out []float64
		for i := head + 1; i <= head+4 && i < len(st.lines); i++ {
			row := app.lastRows[st.rect.Min.Y+i-st.top]
			if strings.TrimSpace(hostapi.SegsText(row)) == "│" {
				break // the air row framing the block ends it
			}
			if row[0].Fade != 0 {
				t.Fatalf("the rail bar sinks: %+v", row[0])
			}
			out = append(out, row[1].Fade)
		}
		return out
	}
	for _, c := range []struct {
		name string
		text string
		want []float64
	}{
		{"a tail that fits keeps its colors", "one line of thought", []float64{0}},
		{"one hidden line eases the ramp", "a\nb\nc\nd\ne", []float64{0.2, 0.15, 0.1, 0.05}},
		{"four hidden lines sink it fully", "a\nb\nc\nd\ne\nf\ng\nh", []float64{0.8, 0.6, 0.4, 0.2}},
		{"more hidden lines sink no deeper", strings.Repeat("a line of thought\n", 12), []float64{0.8, 0.6, 0.4, 0.2}},
	} {
		got := tail(c.text)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %d rows, want %d: %v", c.name, len(got), len(c.want), got)
		}
		for i := range c.want {
			if math.Abs(got[i]-c.want[i]) > 1e-9 {
				t.Fatalf("%s: %v, want %v", c.name, got, c.want)
			}
		}
	}
	// a row the widget sank deeper keeps its sink: the region's edge is shallower
	row := anim.Faded([]hostapi.Segment{{Text: "a", Fade: 0.9}, {Text: "b", Bar: true}}, 0.4, spareBar)
	if row[0].Fade != 0.9 || row[1].Fade != 0 {
		t.Fatalf("the region brightened a sunk row: %+v", row)
	}
}

func TestChatStickyDigest(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	host.AppendMessage("user", "look around")
	appendReasoning(host, "where", hostapi.TurnStat{ThoughtMS: 8000})
	for i := range 12 {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "ls", Label: fmt.Sprintf("list dir %d", i), Status: "ok"})
	}
	for i := range 12 {
		appendReply(host, fmt.Sprintf("answer %d", i), hostapi.TurnStat{})
	}
	const prompt, digest = "▌ look around", "▾ 12 commands"
	var st *scrollState
	// pins scrolls the chat so the first row holding at tops the view, and
	// reads the pins drawn over it, each on the panel running on to the bar
	pins := func(at string) []string {
		app.renderSync(t)
		app.Frame()
		st = app.scrolls[_regionChat]
		i := slices.IndexFunc(st.lines, func(r []hostapi.Segment) bool { return strings.Contains(hostapi.SegsText(r), at) })
		if i < 0 {
			t.Fatal("no such row")
		}
		app.scrollRows(_regionChat, i-st.top)
		app.Frame()
		var out []string
		for _, row := range app.lastRows[st.rect.Min.Y:] {
			if row[0].HL != "Pinned" {
				break
			}
			if row[0].Fill != hostapi.FillPanel || row[len(row)-2].Fill != hostapi.FillPanel {
				t.Fatalf("a pin off the panel: %+v", row)
			}
			out = append(out, hostapi.SegsText(row))
		}
		return out
	}
	app.renderSync(t)                                 // the first render forgets what was opened before it
	for _, id := range []string{"turn:2", "more:2"} { // the run open, and whole
		clickChat(t, app, id)
	}
	for _, c := range []struct {
		at   string
		want []string
	}{
		{"list dir 5", []string{prompt, digest}},
		{"list dir 9", []string{prompt, digest}},
		{"list dir 11", []string{prompt}},
		{"answer 2", []string{prompt}},
	} {
		if got := pins(c.at); !slices.EqualFunc(got, c.want, strings.HasPrefix) {
			t.Fatalf("at %q the pins read %q, want %q", c.at, got, c.want)
		}
	}

	pins("list dir 5")
	clickAt(app, 5, st.rect.Min.Y+1)
	app.Frame()
	if row := app.lastRows[st.rect.Min.Y+1]; row[0].HL == "Pinned" || !strings.HasPrefix(hostapi.SegsText(row), digest) || !headerAt(st.lines, st.top+1) {
		t.Fatalf("the click on the digest's pin left top %d with %q under the prompt", st.top, hostapi.SegsText(row))
	}

	clickChat(t, app, "turn:2")
	if got := pins("answer 2"); !slices.EqualFunc(got, []string{prompt}, strings.HasPrefix) {
		t.Fatalf("with the run folded the pins read %q", got)
	}
}
