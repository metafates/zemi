package ui

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	zemiruntime "github.com/metafates/zemi/runtime"
)

// TestUnitAt: a row whose own anchor is not drawn belongs to the nearest unit
// before it (a notice folded into its burst, a call inside a run's digest),
// and a row above every unit belongs to none.
func TestUnitAt(t *testing.T) {
	lines := [][]hostapi.Segment{
		{{Text: ""}},
		{{Text: "a", Anchor: 1}},
		{{Text: "a", Anchor: 1}}, // a wrapped row carries it again
		{{Text: ""}},
		{{Text: "b", Anchor: 4}},
	}
	for _, c := range []struct{ at, want int }{
		{1, 1}, {2, 1}, {3, 1}, {4, 4}, {5, 4}, {0, 0},
	} {
		if got := unitAt(lines, c.at); got != c.want {
			t.Errorf("anchor %d: unit %d, want %d", c.at, got, c.want)
		}
	}
	if got := unitAt(nil, 3); got != 0 {
		t.Fatalf("nothing drawn: unit %d", got)
	}
}

// banded reports whether a composed row carries the picker's band.
func banded(s hostapi.Segment) bool { return s.Fill == hostapi.FillBand }

// TestMessagesHomeLoadsPages: home at the top of the list loads the pages the
// window shed, the way a page-up does, so the log's start is reachable from a
// fresh /messages without walking up row by row.
func TestMessagesHomeLoadsPages(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	dirs := config.Dirs{StateHome: resolved(t, t.TempDir()), DataHome: resolved(t, t.TempDir()), ProjectDir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(dirs.ProjectDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(dirs.ProjectDir)
	if err != nil {
		t.Fatal(err)
	}
	_, seed, err := store.OpenForDirs(dirs.StateHome, root)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1100 {
		if _, err := seed.AppendLog(store.KindUser, fmt.Sprintf("message %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	_, cfg := tempHome(t)
	rt := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: lua.PathClasses{{Prefix: cfg, Origin: lua.OriginUser}}, RuntimeFS: zemiruntime.FS, Clock: tipClock})
	host := hostapi.NewHost(rt, log, config.NewStore(), config.Dirs{ConfigHome: cfg, DataHome: dirs.DataHome, StateHome: dirs.StateHome, ProjectDir: dirs.ProjectDir})
	host.SetProjectTrusted(true)
	app := NewApp(host, log)
	host.SetUIHooks(app)
	app.CaptureHooks()
	if err := rt.Boot(t.Context(), []lua.BootStep{host.NamespacesStep(), lua.RequireStep("zemi.tui")}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome}) // to the first row
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome}) // held there, so it loads
	o := app.overlays[len(app.overlays)-1].o
	if o.Items[0].Section != "Earlier" {
		t.Fatalf("home loaded %d rows, first section %q", len(o.Items), o.Items[0].Section)
	}
	if len(o.Items) != len(host.Session().Transcript)+100 {
		t.Fatalf("the extended list holds %d rows", len(o.Items))
	}
}

// bandedChatRows is the text of the chat rows the picker's message is banded
// on, as the last frame drew them.
func bandedChatRows(app *App) []string {
	chat := app.scrolls["chat"]
	var out []string
	for y := chat.rect.Min.Y; y < min(chat.rect.Max.Y, len(app.lastRows)); y++ {
		// the chat's first cell, so a box drawn over the rows is no band here
		if row := dropCells(app.lastRows[y], chat.rect.Min.X); len(row) > 0 && banded(row[0]) {
			out = append(out, strings.TrimSpace(hostapi.SegsText(app.lastRows[y])))
		}
	}
	return out
}

// TestMessagesRowsTrimAtTheBox: a row the popup can show whole is not cut,
// and one wider than it is cut once, at the box's width, wide runes included.
func TestMessagesRowsTrimAtTheBox(t *testing.T) {
	for _, c := range []struct {
		name, text string
		wantCut    bool
	}{
		{"fits whole", strings.Repeat("a", 70), false},
		{"wider than the box", strings.Repeat("a", 220), true},
		{"wide runes wider than the box", strings.Repeat("汉", 80), true},
	} {
		app, host := bootAgentApp(t, 120, 30)
		host.AppendMessage("user", c.text)
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		st := &app.overlays[len(app.overlays)-1]
		box := app.overlayBox(st, 120, 30, true)
		for i, r := range box {
			if i > 2 {
				break
			}
			t.Logf("%s: row %d w=%d size=%+v", c.name, i, hostapi.SegsWidth(r), st.size)
		}
		// the message's row, under the section header the flat list draws
		row := box[slices.IndexFunc(box, func(r []hostapi.Segment) bool {
			return strings.Contains(hostapi.SegsText(r), "›")
		})]
		text, width := hostapi.SegsText(row), hostapi.SegsWidth(row)
		t.Logf("%s: boxw=%d roww=%d", c.name, st.size.w, width)
		if n := strings.Count(text, "…"); n != map[bool]int{true: 1, false: 0}[c.wantCut] {
			t.Fatalf("%s: %d ellipses in %q", c.name, n, text)
		}
		if c.wantCut && width <= 68 { // the old 60-rune clip drew 68 cells here
			t.Fatalf("%s: cut at %d cells, want the box's width: %q", c.name, width, text)
		}
	}
}

// fillOneTurn fills one turn: its prompt and 30 replies, so a turn's own
// rows are a long conversation, each one a message the chat draws.
func fillOneTurn(host *hostapi.Host) {
	host.AppendMessage("user", "message 1")
	for i := 1; i <= 30; i++ {
		appendReply(host, fmt.Sprintf("reply %d", i), hostapi.TurnStat{In: 1000, Out: 100, ElapsedMS: 1500})
	}
}

// openMessages opens the flat list: the level a message is picked at, where
// every row names its own transcript row.
func openMessages(t *testing.T, app *App, host *hostapi.Host) *overlayState {
	t.Helper()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	if len(app.overlays) != 1 || !app.overlays[0].o.Follow {
		t.Fatalf("overlays: %+v", app.overlays)
	}
	return &app.overlays[0]
}

// TestMessagesOpenAtGap: with the chat at its top, or with the view's first
// row the blank line between two messages, the picker opens on the message
// the reader sees, never on the one already scrolled out of view above.
func TestMessagesOpenAtGap(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	open := func() {
		t.Helper()
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		if len(app.overlays) == 0 {
			t.Fatal("no overlays")
		}
		from, to := unitRows(chat.lines, app.overlays[len(app.overlays)-1].selectedAnchor())
		if from < chat.top || from >= chat.top+chat.viewH {
			t.Fatalf("the picked message is off screen: rows %d..%d, view %d..%d", from, to, chat.top, chat.top+chat.viewH)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
		app.Frame()
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome, Mod: tea.ModCtrl})
	app.Frame()
	if chat.top != 0 {
		t.Fatalf("not at the top: %d", chat.top)
	}
	open()

	// a blank row between two messages at the view's top, the message above
	// it ends before the view starts
	delete(app.pendingScroll, "chat")
	for top := chat.rows - chat.viewH; top > 0; top-- {
		chat.top = top
		if rowAnchor(chat.lines[top]) == 0 && rowAnchor(chat.lines[top+1]) > 0 {
			break
		}
	}
	app.Frame()
	if chat.top == 0 || rowAnchor(chat.lines[chat.top]) != 0 || rowAnchor(chat.lines[chat.top+1]) == 0 {
		t.Fatalf("no gap at the top: top %d", chat.top)
	}
	open()
}

// TestMessagesCallStepsTheRail: a call view opened from a rail steps along
// it, and the rail steps with it, so going back from the view lands on the
// call that was read and the chat bands it.
func TestMessagesCallStepsTheRail(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 110, 34)
	reloadUI(t, host)
	host.AppendMessage("user", "walk the rail")
	for i := 1; i <= 4; i++ {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: fmt.Sprintf("f%d.go", i), Label: fmt.Sprintf("read %d", i), Status: "ok"})
	}
	appendReply(host, "all green", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	pump := func() {
		for range 3 {
			for app.Pump() {
			}
			app.Frame()
			app.renderSync(t)
		}
		app.Frame()
	}
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	label := func() string { return top().items[top().visible[top().selected]].label }
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"}) // the rail row
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"}) // the rail level
	pump()
	press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"}) // read 2
	pump()
	if got := label(); got != "read 2" {
		t.Fatalf("the rail opened on %q", got)
	}
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"}) // its view
	pump()
	if id := top().o.ID; id != "call:3" {
		t.Fatalf("the view is %q", id)
	}
	press(t, app, tea.KeyPressMsg{Code: 'j', Mod: tea.ModCtrl}) // step to read 3
	pump()
	if id := top().o.ID; id != "call:4" {
		t.Fatalf("the stepped view is %q", id)
	}
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	pump()
	if id, got := top().o.ID, label(); id != "messages rail 2" || got != "read 3" {
		t.Fatalf("back on %q with %q selected, want read 3", id, got)
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "read 3") {
		t.Fatalf("banded %q, want the call the view stepped to", got)
	}
}

// TestMessagesRailKeepsThePick: /messages opened on a run's digest draws that
// run whole, and the run grows under the row its level names: the pick stays
// on screen, the view growing down with the run instead of sliding to the
// tail with its top carried off.
func TestMessagesRailKeepsThePick(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 24)
	for i := range 4 {
		host.AppendMessage("user", fmt.Sprintf("question %d", i))
		appendReply(host, strings.Repeat("a reply that wraps. ", 10), hostapi.TurnStat{})
	}
	host.AppendMessage("user", "a long run please")
	for i := 1; i <= 20; i++ {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: fmt.Sprintf("f%d.go", i), Label: fmt.Sprintf("read %d", i), Status: "ok"})
	}
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	// the chat sits at its tail, the run the last thing it draws folded
	if !chat.follow {
		t.Fatalf("the chat is not at its tail: top %d, %d rows", chat.top, chat.rows)
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		for app.Pump() {
		}
		app.Frame()
		app.renderSync(t)
	}
	app.Frame()
	top := &app.overlays[len(app.overlays)-1]
	if !strings.HasSuffix(top.o.ID, " rail 10") || top.o.Rail != 10 {
		t.Fatalf("opened %q, want the run's level", top.o.ID)
	}
	pick := top.selectedAnchor()
	from, to := unitRows(chat.lines, pick)
	if from < 0 || from < chat.top || to >= chat.top+chat.viewH {
		t.Fatalf("the picked call %d is off screen: rows %d..%d of view %d..%d", pick, from, to, chat.top, chat.top+chat.viewH)
	}
	if chat.rows <= 28 {
		t.Fatalf("the run did not grow under the pick: %d rows", chat.rows)
	}
	if chat.follow {
		t.Fatal("the view slid to the tail instead of staying on the pick")
	}
}

// TestMessagesOpenOnBlank: the blank row a message is set off from the one
// before it by is no message either, and it hands over to the message under
// it - the row the chat names there - so a reader who scrolled past a prompt
// opens on the reply, never back on that prompt.
func TestMessagesOpenOnBlank(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 12)
	fillChat(host)
	host.AppendMessage("user", "the prompt")
	appendReply(host, strings.Repeat("the reply wraps on and on. ", 40), hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	prompt := -1
	for i, row := range chat.lines {
		if strings.HasPrefix(hostapi.SegsText(row), "▌ the prompt") {
			prompt = i
		}
	}
	if prompt < 0 || prompt+2 >= len(chat.lines) || rowAnchor(chat.lines[prompt+1]) != 0 || rowAnchor(chat.lines[prompt+2]) == 0 {
		t.Fatalf("no blank row under the prompt: %d of %d lines", prompt, len(chat.lines))
	}
	if chat.rows <= chat.viewH {
		t.Fatalf("the chat does not scroll: %d rows, %d high", chat.rows, chat.viewH)
	}
	// the blank is the row the view's middle draws
	delete(app.pendingScroll, "chat")
	chat.top, chat.focus, chat.follow = max(prompt+1-(chat.viewH-1)/2, 0), 0, false
	app.Frame()
	place := app.chatRow()
	if place == 0 || place == 1 {
		t.Fatalf("the chat names row %d at the blank under the prompt", place)
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	top := &app.overlays[len(app.overlays)-1]
	if got := top.selectedAnchor(); got != place {
		t.Fatalf("opened on row %d (%q), the chat is at %d", got, top.items[top.visible[top.selected]].label, place)
	}
	if got := top.items[top.visible[top.selected]].label; !strings.Contains(got, "the reply") {
		t.Fatalf("opened on %q, want the reply under the blank", got)
	}
}

// TestMessagesOpenOnRun: a run of calls is one row the list names, its
// digest, so a view whose top row is the digest opens on it - never back on
// the reply above, which may be many rows away.
func TestMessagesOpenOnRun(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 12)
	host.AppendMessage("user", "one")
	var long strings.Builder
	for i := range 20 {
		fmt.Fprintf(&long, "line %d\n\n", i)
	}
	appendReply(host, long.String(), hostapi.TurnStat{})
	for _, tool := range []struct{ name, text, label string }{
		{"read", "a.go", "read a"}, {"glob", "*.go", "find files"}, {"shell", "go test", "run the tests"},
	} {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: tools.ToolName(tool.name), Text: tool.text, Label: tool.label, Status: "ok"})
	}
	appendReply(host, "reply two", hostapi.TurnStat{})
	// a long tail, so the run's digest can stand at the view's top
	host.AppendMessage("user", "three")
	appendReply(host, long.String(), hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	digest := -1
	for i, row := range chat.lines {
		if strings.HasPrefix(hostapi.SegsText(row), "· ") {
			digest = i
		}
	}
	if digest < 0 || chat.rows <= chat.viewH {
		t.Fatalf("no digest row to scroll to, or the chat does not scroll: %d, %d rows", digest, chat.rows)
	}
	delete(app.pendingScroll, "chat")
	// the digest is the row the view's middle draws
	chat.top, chat.focus, chat.follow = max(digest-(chat.viewH-1)/2, 0), 0, false
	app.Frame()
	if got := app.chatRow(); got == 0 {
		t.Fatalf("no place over the digest at %d, chat top %d", digest, chat.top)
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	ov := &app.overlays[0]
	if got := ov.selectedAnchor(); got != 3 { // the digest, the run's first row
		t.Fatalf("opened on %d (%q), want the run the digest folds", got, ov.items[ov.visible[ov.selected]].label)
	}
}

// TestMessagesBandKeepsScrollbar: the scrollbar is not part of the message a
// picker bands, so its own cell keeps the canvas under the band.
func TestMessagesBandKeepsScrollbar(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	host.AppendMessage("user", "long one")
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "line %d\n\n", i)
	}
	appendReply(host, b.String(), hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	chat := app.scrolls["chat"]
	if !chat.overflows() {
		t.Fatal("the chat should scroll")
	}
	from, _ := unitRows(chat.lines, app.overlays[0].selectedAnchor())
	cell := dropCells(app.lastRows[chat.rect.Min.Y+from-chat.top], chat.rect.Max.X-1)[0]
	if !cell.Bar || cell.Fill == hostapi.FillBand {
		t.Fatalf("the scrollbar took the band: %+v", cell)
	}
}

// TestMessagesTallJump: a message taller than the view starts just under the
// pinned header above it, instead of being drawn over by it.
func TestMessagesTallJump(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillChat(host)
	// the transcript ends on a tall prompt, so the list's last item is it
	var b strings.Builder
	for i := range 40 {
		fmt.Fprintf(&b, "line %d\n\n", i)
	}
	host.AppendMessage("user", "long one\n\n"+b.String())
	app.renderSync(t)
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	app.Frame()
	chat := app.scrolls["chat"]
	ov := &app.overlays[len(app.overlays)-1]
	// away and back, so the jump to this prompt is what the second key makes
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnd})
	app.Frame()
	from, to := unitRows(chat.lines, ov.selectedAnchor())
	if from < 0 || to-from < chat.viewH {
		t.Fatalf("the picked message is not taller than the view: %d..%d", from, to)
	}
	pins := len(pinned(nil, chat.lines, chat.top))
	if from != chat.top+pins {
		t.Fatalf("the message starts at view row %d, %d pins over it", from-chat.top, pins)
	}
}

// TestMessagesFollow: the messages picker opens on the row the reader is at,
// moving its selection brings that message's first row to the top of the
// chat and bands it, and a row in view at the tail leaves the chat following
// it.
func TestMessagesFollow(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 14)
	fillOneTurn(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	if chat == nil || !chat.follow {
		t.Fatal("the chat should follow its tail")
	}
	open := func() *overlayState {
		t.Helper()
		for app.Pump() {
		}
		return openMessages(t, app, host)
	}
	ov := open()
	if got := ov.selectedAnchor(); got != 31 { // the last reply, at the tail
		t.Fatalf("opened on anchor %d", got)
	}
	if top := chat.top; top != chat.rows-chat.viewH {
		t.Fatalf("opening moved the chat: top %d of %d, view %d", top, chat.rows, chat.viewH)
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	if chat.follow {
		t.Fatal("a selection above the tail should leave it")
	}
	if chat.top != 0 { // the whole chat is under the first message already
		t.Fatalf("the first message should keep top 0, got %d", chat.top)
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "message 1") {
		t.Fatalf("banded rows %q", got)
	}

	// a message well inside the chat lands in the middle of the view
	ov.selected = 21 // reply 21
	app.Frame()
	from, to := unitRows(chat.lines, ov.selectedAnchor())
	if from < chat.top || to >= chat.top+chat.viewH {
		t.Fatalf("the message is off screen: rows %d..%d, view %d..%d", from, to, chat.top, chat.top+chat.viewH)
	}
	if mid := (chat.viewH - (to - from + 1)) / 2; from-chat.top != mid {
		t.Fatalf("the message starts at view row %d, want it centered at %d", from-chat.top, mid)
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "reply 21") {
		t.Fatalf("banded rows %q", got)
	}

	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnd})
	app.Frame()
	if !chat.follow || chat.top != chat.rows-chat.viewH {
		t.Fatal("the last row should hand the chat back to its tail")
	}
	// the readout under the reply is meta: never banded
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "reply 30") || strings.Contains(got[0], "out") {
		t.Fatalf("banded rows %q", got)
	}
	// a row already in the tail's view keeps the chat on the tail
	press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
	app.Frame()
	if !chat.follow {
		t.Fatal("a row in view at the tail should keep following it")
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	app.Frame()
	if !chat.follow || chat.top != chat.rows-chat.viewH {
		t.Fatalf("esc should put a following chat back on its tail: top %d of %d", chat.top, chat.rows)
	}

	// scrolled up, the picker opens on the message the top row belongs to and
	// esc puts the chat back where it was
	for range 3 {
		press(t, app, tea.KeyPressMsg{Code: tea.KeyPgUp})
	}
	app.Frame()
	top := chat.top
	ov = open()
	from, to = unitRows(chat.lines, ov.selectedAnchor())
	if from < 0 || from >= chat.top+chat.viewH || to < chat.top {
		t.Fatalf("opened on an off-screen message: rows %d..%d, view %d..%d", from, to, chat.top, chat.top+chat.viewH)
	}
	app.Frame()
	if chat.top != top {
		t.Fatalf("opening moved the chat: %d -> %d", top, chat.top)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	if chat.top == top {
		t.Fatal("a jump should move the chat")
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "message 1") {
		t.Fatalf("banded rows %q", got)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	app.Frame()
	if chat.top != top || len(app.overlays) != 0 {
		t.Fatalf("esc should cancel: top %d, want %d, %d overlays", chat.top, top, len(app.overlays))
	}
	if got := bandedChatRows(app); len(got) != 0 {
		t.Fatalf("banded rows after esc: %q", got)
	}

	// enter keeps the place instead
	open()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyHome})
	app.Frame()
	jumped := chat.top
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	app.Frame()
	if len(app.overlays) != 0 {
		t.Fatalf("%d overlays after enter", len(app.overlays))
	}
	if chat.top != jumped || chat.top == top {
		t.Fatalf("enter should keep the jump: top %d, jumped %d, was %d", chat.top, jumped, top)
	}
	if got := bandedChatRows(app); len(got) != 0 {
		t.Fatalf("banded rows after enter: %q", got)
	}
}

// TestMessagesOpensAtTheReader: /messages opens on the message the reader is
// at, and enter there closes the menu keeping the place it moved the chat to.
func TestMessagesOpensAtTheReader(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 120, 46)
	fillChat(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	open := func() *overlayState {
		t.Helper()
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		if len(app.overlays) != 1 {
			t.Fatalf("%d overlays, want the list alone", len(app.overlays))
		}
		return &app.overlays[len(app.overlays)-1]
	}
	// every turn's prompt, put at the top of the view, opens on it
	turns := 0
	for _, line := range chat.lines {
		anchor := rowAnchor(line)
		if !strings.Contains(hostapi.SegsText(line), "message ") || anchor == 0 {
			continue
		}
		var from int
		for i, l := range chat.lines {
			if rowAnchor(l) == anchor {
				from = i
				break
			}
		}
		chat.follow, chat.top, chat.focus = false, from, 0
		app.Frame()
		ov := open()
		if ov.o.ID != "messages" {
			t.Fatalf("the reader at %d opened %q", anchor, ov.o.ID)
		}
		if got := ov.selectedAnchor(); got != anchor && !strings.Contains(fmt.Sprint(ov.labels), "message") {
			t.Fatalf("the reader at %d opened on %d", anchor, got)
		}
		want := ov.selectedAnchor()
		top := chat.top
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
		app.Frame()
		if len(app.overlays) != 0 {
			t.Fatalf("enter left %d overlays", len(app.overlays))
		}
		if chat.top != top {
			t.Fatalf("enter moved the chat: top %d, want %d", chat.top, top)
		}
		// reopening lands on the same row again
		if got := open().selectedAnchor(); got != want {
			t.Fatalf("reopened on %d, want %d", got, want)
		}
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
		app.Frame()
		turns++
	}
	if turns < 5 {
		t.Fatalf("only %d turns read", turns)
	}
}

// TestMessagesBottomDropsPlace: scrolling to the bottom drops where a jump
// put the view, even when the chat is already pinned to its tail and the
// scroll moves nothing: the picker then opens on the last message.
func TestMessagesBottomDropsPlace(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 80, 20)
	fillOneTurn(host)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	open := func() *overlayState {
		t.Helper()
		if err := host.OpenMessages(); err != nil {
			t.Fatal(err)
		}
		for app.Pump() {
		}
		return &app.overlays[0]
	}
	// a turn's own rows: a jump there, then enter keeps that place
	rows := openMessages(t, app, host)
	rows.selected = 21 // reply 21
	app.Frame()
	if chat.focus != 22 {
		t.Fatalf("follow %v, focus %d", chat.follow, chat.focus)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	app.Frame()
	// the chat is already at its tail: this scroll moves nothing
	press(t, app, tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	app.Frame()
	if chat.focus != 0 {
		t.Fatalf("a scroll to the bottom kept the place: focus %d", chat.focus)
	}
	if got := open().selectedAnchor(); got != 31 {
		t.Fatalf("opened on %d, want the last reply 31", got)
	}
}

// TestMessagesWheelKeepsPlace: the wheel moves the chat a few lines at a
// time. A line or two leaves the message a jump centered near the middle, so
// the list opens on it again; scrolled well past it, the list opens on the
// message the middle now shows.
func TestMessagesWheelKeepsPlace(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 148, 44)
	host.AppendMessage("user", "prompt")
	for i := range 20 {
		appendReply(host, fmt.Sprintf("reply %d at\none\ntwo\nthree\nfour", i), hostapi.TurnStat{})
	}
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	ov := openMessages(t, app, host)
	ov.selected = slices.IndexFunc(ov.visible, func(idx int) bool {
		return strings.Contains(ov.items[idx].label, "reply 10 at")
	})
	if ov.selected < 0 {
		t.Fatal("no reply 10 in the list")
	}
	app.Frame()
	want := ov.selectedAnchor()
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	app.Frame()
	if chat.follow {
		t.Fatal("the chat should sit off its tail")
	}
	wheel := func(n int) {
		for range n {
			app.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown, X: 40, Y: 10})
		}
		app.Frame()
	}
	for range 2 {
		wheel(1)
		got := openMessages(t, app, host).selectedAnchor()
		press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
		app.Frame()
		if got != want {
			t.Fatalf("a wheel line: opened on %d, want %d", got, want)
		}
	}
	wheel(8)
	got := openMessages(t, app, host).selectedAnchor()
	if got == want {
		t.Fatalf("scrolled well past it, still on %d", want)
	}
	mid := chat.top + (chat.viewH-1)/2
	if from, to := unitRows(chat.lines, got); from > mid || to < mid {
		t.Fatalf("opened on rows %d..%d, the view's middle is %d", from, to, mid)
	}
}

// messagesApp boots an app whose host knows /messages, which the ui tests
// otherwise boot without: the tree's rows are commands.
func messagesApp(t *testing.T) (*App, *hostapi.Host) {
	t.Helper()
	app, host := bootAgentApp(t, 110, 34)
	if _, err := host.Commands.Add(&hostapi.Command{
		Name: "messages", NArgs: hostapi.NArgsAny,
		GoFn: func(h *hostapi.Host, args string, confirm bool) error { return h.MessagesCommand(args) },
	}); err != nil {
		t.Fatal(err)
	}
	// a row's o opens the view a screen would: one that does not follow the
	// chat, drawn over the list it came from
	runLua(t, app, `zemi.command.add("call", function(args)
		zemi.ui.overlay.open({ id = "call:" .. args.args, title = "Command", body = "status ok" })
	end, { nargs = "*" })`)
	return app, host
}

// TestMessagesRailClosesOnARenamedNode: the close handshake matches the chat
// by the region it draws, not the scroll node's id, so a layout that renames
// the node still folds the run and closes the list.
func TestMessagesRailClosesOnARenamedNode(t *testing.T) {
	t.Parallel()
	app, host := messagesApp(t)
	runLua(t, app, `zemi.ui.layout.set({ root = { kind = "box", dir = "col", stage = true, children = {
		{ kind = "region", id = "transcript", region = "chat", scroll = "bottom", size = "fill" },
		{ kind = "composer" },
	} } })`)
	host.AppendMessage("user", "walk a running rail")
	for i := range 3 {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: tools.ToolName("read"), Text: fmt.Sprintf("f%d.go", i), Label: fmt.Sprintf("read f%d", i), Status: "ok"})
	}
	appendReply(host, "all green", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	app.openOverlay(hostapi.Overlay{
		ID: "messages turn 1 rail 2", Title: "rail", Origin: lua.OriginBuiltin, Follow: true, Rail: 2,
		Items: []hostapi.OverlayItem{{Text: "run", Anchor: 2}},
	})
	app.renderSync(t)
	if frame := ansi.Strip(app.Frame()); !strings.Contains(frame, "read f1") {
		t.Fatalf("the rail did not open the run whole:\n%s", frame)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	if app.railClose == "" || len(app.overlays) != 1 {
		t.Fatalf("esc did not arm the close: closing %q, %d overlays", app.railClose, len(app.overlays))
	}
	for range 3 {
		for app.Pump() {
		}
		app.renderSync(t)
		app.Frame()
	}
	if app.railClose != "" || len(app.overlays) != 0 {
		t.Fatalf("a renamed scroll node stranded the closing rail: closing %q, %d overlays", app.railClose, len(app.overlays))
	}
	if frame := ansi.Strip(app.Frame()); strings.Contains(frame, "read f1") {
		t.Fatalf("the run stayed open:\n%s", frame)
	}
}

// TestMessagesMenu: /messages is one list over the chat; l reads a row whole
// and esc there returns to the list as it was, enter closes the menu keeping
// the chat's place, and esc closes whole with the chat back where the menu
// found it.
func TestMessagesMenu(t *testing.T) {
	t.Parallel()
	app, host := messagesApp(t)
	host.AppendMessage("user", "first")
	appendReply(host, "first reply", hostapi.TurnStat{})
	host.AppendMessage("user", "second")
	appendReply(host, "second reply with text", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	pump := func() {
		for app.Pump() {
		}
		app.Frame()
	}
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	ids := func() []string {
		var out []string
		for _, st := range app.overlays {
			out = append(out, st.o.ID)
		}
		return out
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	pump()
	// the reader sits at the tail: the list alone, on the last message
	if n, id := len(app.overlays), top().o.ID; n != 1 || id != "messages" {
		t.Fatalf("%d overlays, top %q", n, id)
	}
	want := top().selectedAnchor()
	// l reads the message over the list; esc comes back to it as it was
	row := top().selected
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	waitPrompt(t, app, func() bool { return strings.HasPrefix(top().o.ID, "messages view ") })
	pump()
	if id := top().o.ID; !strings.HasPrefix(id, "messages view ") {
		t.Fatalf("l opened %q: %v", id, ids())
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n, id := len(app.overlays), top().o.ID; n != 1 || id != "messages" || top().selected != row {
		t.Fatalf("esc on the view: %d overlays, %q at %d, want %d: %v", n, id, top().selected, row, ids())
	}
	// enter is the place itself: the menu closes
	chat := app.scrolls["chat"]
	chatTop := chat.top
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("enter left %v", ids())
	}
	if chat.top != chatTop {
		t.Fatalf("enter moved the chat: top %d, want %d", chat.top, chatTop)
	}
	if got := bandedChatRows(app); len(got) != 0 {
		t.Fatalf("banded rows after enter: %q", got)
	}
	// reopening the menu follows the chat again, from the place it kept
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	pump()
	if got := top().selectedAnchor(); got != want {
		t.Fatalf("reopened on %d, want %d", got, want)
	}
	// esc closes the whole menu, the chat back where the menu found it
	found := chat.top
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("esc left %d overlays", n)
	}
	if chat.top != found {
		t.Fatalf("esc left the chat at %d, it was found at %d", chat.top, found)
	}
}

// TestMessagesFollowsTheTranscript: a row that lands while the picker is open
// shows there, on the level that holds it, and the selection stays.
func TestMessagesFollowsTheTranscript(t *testing.T) {
	t.Parallel()
	app, host := messagesApp(t)
	host.AppendMessage("user", "watch this")
	appendReply(host, "first reply", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	for app.Pump() {
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		for app.Pump() {
		}
		app.Frame()
	}
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	before := len(top().visible)
	sel := top().selected
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "notice", Text: "a notice landed"})
	for range 4 {
		for app.Pump() {
		}
		app.Frame()
	}
	if got := len(top().visible); got != before+1 {
		t.Fatalf("the level holds %d rows, want %d: %v", got, before+1, top().labels)
	}
	if !slices.Contains(top().labels, "a notice landed") {
		t.Fatalf("the new row is not in the level: %v", top().labels)
	}
	if top().selected != sel {
		t.Fatalf("the selection moved to %d, want %d", top().selected, sel)
	}
}

// TestMessagesRail: a turn's activity is one row in the turn view, the rail
// the chat folds; l opens its calls, the chat draws that run whole and bands
// the call the list names, and going back folds it again.
func TestMessagesRail(t *testing.T) {
	t.Parallel()
	app, host := messagesApp(t)
	host.AppendMessage("user", "walk the rail")
	for _, tool := range []struct{ name, text, label string }{
		{"read", "a.go", "read a"},
		{"grep", "Parse", "find Parse"},
		{"shell", "go test ./...", "run the tests"},
	} {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: tools.ToolName(tool.name), Text: tool.text, Label: tool.label, Status: "ok"})
	}
	appendReply(host, "all green", hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	pump := func() {
		for range 3 {
			for app.Pump() {
			}
			app.Frame() // a compose settles the follow state the widget reads
			app.renderSync(t)
		}
		app.Frame()
	}
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	frame := func() string { return ansi.Strip(app.Frame()) }
	// folded, the run is its digest alone: what it counted, then the first
	// call's about; the later calls' labels stay behind the fold
	if !strings.Contains(frame(), "1 file, 1 pattern, 1 command · read a") || strings.Contains(frame(), "find Parse") {
		t.Fatalf("the settled run is not folded:\n%s", frame())
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	pump()
	// the turn's activity is one row: the run folds into it
	press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
	pump()
	if got := top().items[top().visible[top().selected]].row.Text; got != "3 steps" {
		t.Fatalf("the turn's selected row is %q, want the run's", got)
	}
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	if id, n := top().o.ID, len(top().visible); id != "messages rail 2" || n != 3 {
		t.Fatalf("rail view: %q, %d rows", id, n)
	}
	// the run draws whole, and only the picked call's row is banded
	for _, want := range []string{"read a", "find Parse", "run the tests"} {
		if !strings.Contains(frame(), want) {
			t.Fatalf("the rail did not unfold %q:\n%s", want, frame())
		}
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "read a") {
		t.Fatalf("banded %q, want the picked call alone", got)
	}
	press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"})
	pump()
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "find Parse") {
		t.Fatalf("banded %q after a step, want the next call", got)
	}
	// l opens the call's own screen over the list: the chat keeps the run
	// open and bands the row under it, and esc comes back to the same preview
	row := top().selected
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	waitPrompt(t, app, func() bool { return top().o.ID == "call:3" })
	pump()
	if id := top().o.ID; id != "call:3" || !strings.Contains(frame(), "read a") {
		t.Fatalf("l opened %q with the run folded:\n%s", id, frame())
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "find Parse") {
		t.Fatalf("banded %q under the call's view, want the picked call", got)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n, id := len(app.overlays), top().o.ID; n != 2 || id != "messages rail 2" || top().selected != row {
		t.Fatalf("esc on the view: %d overlays, %q at %d, want the rail at %d", n, id, top().selected, row)
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.Contains(got[0], "find Parse") {
		t.Fatalf("banded %q after esc, want the picked call", got)
	}
	// enter is the row's place in the chat, never a level down: the menu
	// closes whole on the call it named, the chat keeping that place
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEnter})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("enter on a call left %d overlays", n)
	}
	if got := app.scrolls["chat"].focus; got != 3 {
		t.Fatalf("the chat kept %d as its place, want the call's row 3", got)
	}
	// reopening with the reader in the run brings the rail up with the list
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	pump()
	// h backs the rail out and folds the run again, the band on its one row.
	// The list waits for the chat to render it folded, so no frame shows the
	// run open with the list gone: the fold lands first, the close after it.
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	if app.rail != 0 || app.railClose == "" || len(app.overlays) != 2 {
		t.Fatalf("a closing rail: rail %d, closing %q, %d overlays", app.rail, app.railClose, len(app.overlays))
	}
	pump()
	if strings.Contains(frame(), "find Parse") {
		t.Fatalf("the run stayed open:\n%s", frame())
	}
	if got := bandedChatRows(app); len(got) != 1 || !strings.HasPrefix(got[0], "· ") {
		t.Fatalf("banded %q, want the run's row", got)
	}
	// the rail is a level of the list, so esc on it closes the menu whole
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	pump()
	if n := len(app.overlays); n != 2 {
		t.Fatalf("%d overlays before esc", n)
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	pump()
	if n := len(app.overlays); n != 0 {
		t.Fatalf("esc on the rail left %d overlays", n)
	}
	if app.rail != 0 || strings.Contains(frame(), "find Parse") {
		t.Fatalf("rail %d, the run still open:\n%s", app.rail, frame())
	}
}

// pickLabels is the top overlay's visible rows as "label anchor=… run=…".
func pickLabels(app *App) []string {
	top := &app.overlays[len(app.overlays)-1]
	var out []string
	for _, idx := range top.visible {
		it := top.items[idx]
		out = append(out, fmt.Sprintf("%q anchor=%d run=%q", it.label, it.row.Anchor, it.row.Run))
	}
	return out
}

// walkToList moves the selection of the top overlay to the row whose label
// has sub, one key at a time, pumping between.
func walkToList(t *testing.T, app *App, sub string) {
	t.Helper()
	top := &app.overlays[len(app.overlays)-1]
	at := -1
	for k, idx := range top.visible {
		if strings.Contains(top.items[idx].label, sub) {
			at = k
		}
	}
	if at < 0 {
		t.Fatalf("no row with %q:\n%s", sub, strings.Join(pickLabels(app), "\n"))
	}
	for top.selected != at {
		if top.selected > at {
			press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
		} else {
			press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"})
		}
		for app.Pump() {
		}
	}
}

// bandedChatText is the text of the chat rows the picker's pick is banded on,
// first drawn first, as the last frame composed them.
func bandedChatText(t *testing.T, app *App) []string {
	t.Helper()
	chat := app.scrolls["chat"]
	if chat == nil {
		t.Fatal("no chat scroll")
	}
	var out []string
	for y := chat.rect.Min.Y; y < min(chat.rect.Max.Y, len(app.lastRows)); y++ {
		if row := dropCells(app.lastRows[y], chat.rect.Min.X); len(row) > 0 && banded(row[0]) {
			out = append(out, hostapi.SegsText(app.lastRows[y]))
		}
	}
	return out
}

// TestRailBandCoversTheRun: the list's run row stands for the whole run, so
// the band covers every row the chat draws for it - the digest alone when
// the run folded, the digest and its whole rail when the reader opened it in
// the chat.
func TestRailBandCoversTheRun(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 150, 30)
	host.AppendMessage("user", "one")
	appendReply(host, strings.Repeat("early. ", 80), hostapi.TurnStat{})
	host.AppendMessage("user", "two")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "f1.go", Label: "read f1", Status: "ok"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "f2.go", Label: "read f2", Status: "ok"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "go vet ./...", Label: "vet", Status: "ok"})
	appendReply(host, strings.Repeat("tail padding. ", 80), hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	// the run drew folded: the band is its digest row alone
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	walkToList(t, app, "step")
	app.Frame()
	banded := bandedChatText(t, app)
	if len(banded) != 1 || !strings.Contains(banded[0], "· 2 files") {
		t.Fatalf("folded band:\n%s", strings.Join(banded, "\n"))
	}
	// the reader opens the run in the chat and picks the run row again: the
	// band now covers the digest and every rail row, air rows included
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	for app.Pump() {
	}
	app.renderSync(t)
	x, y := findCell(t, app, "· ")
	clickAt(app, x, y)
	app.renderSync(t)
	app.Frame()
	// /messages opens the run's rail on the call nearest the reader; h backs
	// to the root list, where the run row stands for the whole open rail
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	app.Frame()
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	for range 3 {
		for app.Pump() {
		}
		app.renderSync(t)
		app.Frame()
	}
	walkToList(t, app, "step")
	app.Frame()
	banded = bandedChatText(t, app)
	if len(banded) < 6 || !strings.Contains(banded[0], "2 files") ||
		!slices.ContainsFunc(banded, func(r string) bool { return strings.Contains(r, "read f1") }) ||
		!slices.ContainsFunc(banded, func(r string) bool { return strings.Contains(r, "vet") }) {
		t.Fatalf("whole-rail band:\n%s", strings.Join(banded, "\n"))
	}
	for k, row := range banded {
		if strings.TrimSpace(row) == "" {
			t.Fatalf("row %d of %d banded empty: %q", k, len(banded), row)
		}
	}
}

// TestRailBandGrowsWithTheLiveRail: the run row's band stretches over the
// live tail while the turn adds to it, and a call joining the rail moves the
// band's end down without moving a row above it.
func TestRailBandGrowsWithTheLiveRail(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 24)
	host.AppendMessage("user", "watch the turn")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "npm run dev", Label: "start the dev server", Status: "bg", Job: "job-1"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "job_read", Text: "job-1", Status: "ok"})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{ID: "c3", Name: "shell", Input: "go test ./...", Label: "run the tests", Status: "run"},
	}})
	app.renderSync(t)
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	// the rail level opened on the reader's row; h backs to the root list,
	// where the run row stands for the live rail too
	press(t, app, tea.KeyPressMsg{Code: 'h', Text: "h"})
	for range 3 {
		for app.Pump() {
		}
		app.renderSync(t)
		app.Frame()
	}
	walkToList(t, app, "step")
	app.Frame()
	chat := app.scrolls["chat"]
	drop := func() [][]hostapi.Segment {
		rows := make([][]hostapi.Segment, 0, chat.rect.Dy())
		for y := chat.rect.Min.Y; y < min(chat.rect.Max.Y, len(app.lastRows)); y++ {
			rows = append(rows, dropCells(app.lastRows[y], chat.rect.Min.X))
		}
		return rows
	}
	beforeFrame := drop()
	before := bandedChatText(t, app)
	firstY := -1
	for y, row := range beforeFrame {
		if len(row) > 0 && banded(row[0]) {
			firstY = chat.rect.Min.Y + y
			break
		}
	}
	// the run the turn is still adding to draws whole, so the band covers
	// every row of it: the settled calls and the live tail
	if len(before) < 6 ||
		!slices.ContainsFunc(before, func(r string) bool { return strings.Contains(r, "start the dev server") }) ||
		!slices.ContainsFunc(before, func(r string) bool { return strings.Contains(r, "go test") }) {
		t.Fatalf("live band:\n%s", strings.Join(before, "\n"))
	}
	// another call joins the live rail: the band follows it down and the rows
	// above the run do not move
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{ID: "c3", Name: "shell", Input: "go test ./...", Label: "run the tests", Status: "ok"},
		{ID: "c4", Name: "edit", Input: "f1.go", Label: "fix f1", Status: "run"},
	}})
	app.renderSync(t)
	app.Frame()
	afterFrame := drop()
	after := bandedChatText(t, app)
	if len(after) <= len(before) || !slices.ContainsFunc(after, func(r string) bool { return strings.Contains(r, "fix f1") }) {
		t.Fatalf("grown band (%d rows over %d):\n%s", len(after), len(before), strings.Join(after, "\n"))
	}
	grewY := -1
	for y, row := range afterFrame {
		if len(row) > 0 && banded(row[0]) {
			grewY = chat.rect.Min.Y + y
			break
		}
	}
	if grewY != firstY {
		t.Fatalf("the band's first row moved from %d to %d", firstY, grewY)
	}
	for i := 0; chat.rect.Min.Y+i < firstY; i++ {
		if !slices.Equal(afterFrame[i], beforeFrame[i]) {
			t.Fatalf("row %d above the band moved", chat.rect.Min.Y+i)
		}
	}
}

// TestRailBandCoversOneCall: the rail level's pick is one call, so the band
// covers its head and its detail rows alone - the first action included, the
// digest row and the neighbours never - and a live call bands its live row.
func TestRailBandCoversOneCall(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	host.AppendMessage("user", "walk the rail")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "f1.go", Label: "read f1", Status: "err", Output: "no such file\nstat f1.go: no such file or directory"})
	for i := 2; i <= 3; i++ {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: fmt.Sprintf("f%d.go", i), Label: fmt.Sprintf("read f%d", i), Status: "ok"})
	}
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "shell", Text: "go vet", Label: "vet", Status: "ok"})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{ID: "c5", Name: "grep", Input: "Parse", Label: "find Parse", Status: "run"},
	}})
	app.renderSync(t)
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	pump := func() {
		for range 2 {
			for app.Pump() {
			}
			app.renderSync(t)
			app.Frame()
		}
	}
	pump()
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	label := func() string { it := top().items[top().visible[top().selected]]; return it.label }
	if got := label(); got != "vet" {
		t.Fatalf("opened on %q, want the call at the reader's row", got)
	}
	// the first action bands its head and the detail under it, no more
	for range 4 {
		if top().items[top().visible[top().selected]].label == "read f1" {
			break
		}
		press(t, app, tea.KeyPressMsg{Code: 'k', Text: "k"})
		for app.Pump() {
		}
	}
	pump()
	if got := label(); got != "read f1" {
		t.Fatalf("walked to %q", got)
	}
	// the first action's transcript row is the run's own anchor, so the fade
	// must light it by call: its rows lit, the neighbours dimmed
	faded := func(text string) float64 {
		for _, row := range app.lastRows {
			for _, s := range row {
				if strings.Contains(s.Text, text) {
					return s.Fade
				}
			}
		}
		t.Fatalf("%q not on screen", text)
		return -1
	}
	if f := faded("read f1"); f != 0 {
		t.Fatalf("the pick reads as faded: %v", f)
	}
	if f := faded("read f2"); f != modalFade {
		t.Fatalf("the neighbour escapes the fade: %v", f)
	}
	banded := bandedChatText(t, app)
	if len(banded) != 4 || !strings.Contains(banded[0], "read f1") || !strings.Contains(banded[1], "f1.go") ||
		!strings.Contains(banded[2], "no such file") || !strings.Contains(banded[3], "stat f1.go") {
		t.Fatalf("first action band:\n%s", strings.Join(banded, "\n"))
	}
	// walking down bands each call alone, the digest row clean, and the fade
	// follows: the pick lit, the one before it dimmed again
	prev := ""
	for _, want := range []string{"read f2", "read f3", "vet"} {
		press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"})
		pump()
		banded := bandedChatText(t, app)
		if len(banded) == 0 || !strings.Contains(banded[0], want) || len(banded) > 2 {
			t.Fatalf("pick %q bands:\n%s", want, strings.Join(banded, "\n"))
		}
		for _, row := range banded {
			if strings.HasPrefix(row, "· ") {
				t.Fatalf("the digest row banded for %q:\n%s", want, strings.Join(banded, "\n"))
			}
		}
		if f := faded(want); f != 0 {
			t.Fatalf("pick %q reads as faded: %v", want, f)
		}
		if prev != "" {
			if f := faded(prev); f != modalFade {
				t.Fatalf("%q stays lit under %q: %v", prev, want, f)
			}
		}
		prev = want
	}
	// a live call is a row of its own: walking onto it bands the live rail's
	// row, and the pick stays on screen
	press(t, app, tea.KeyPressMsg{Code: 'j', Text: "j"})
	pump()
	if got := label(); got != "find Parse" {
		t.Fatalf("walked to %q, want the live call", got)
	}
	banded = bandedChatText(t, app)
	if len(banded) != 2 || !strings.Contains(banded[0], "find Parse") || !strings.Contains(banded[1], "Parse") {
		t.Fatalf("live call band:\n%s", strings.Join(banded, "\n"))
	}
	if chat := app.scrolls["chat"]; chat.viewH > 0 {
		from, _ := spanOf(chat.lines, top().selectedAnchor(), true)
		if from < 0 || from < chat.top || from >= chat.top+chat.viewH {
			t.Fatalf("the live pick is off screen: row %d of view %d..%d", from, chat.top, chat.top+chat.viewH)
		}
	}
}

// TestMessagesOpenSelectsTheClosestCall: /messages opens the rail on the
// action nearest where the reader is - the call at or under the view's
// middle when they scrolled, the last settled one at the tail - never the
// rail's first action.
func TestMessagesOpenSelectsTheClosestCall(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	reloadUI(t, host)
	host.AppendMessage("user", "one")
	appendReply(host, strings.Repeat("early. ", 80), hostapi.TurnStat{})
	host.AppendMessage("user", "a long run")
	for i := 1; i <= 8; i++ {
		host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: fmt.Sprintf("f%d.go", i), Label: fmt.Sprintf("read f%d", i), Status: "ok"})
	}
	appendReply(host, strings.Repeat("tail padding. ", 80), hostapi.TurnStat{})
	app.renderSync(t)
	app.Frame()
	// the run drew folded; the digest clicked open, then the view parked with
	// the middle of it on the run's sixth row
	x, y := findCell(t, app, "· ")
	clickAt(app, x, y)
	app.renderSync(t)
	app.Frame()
	chat := app.scrolls["chat"]
	mid := -1
	for i, row := range chat.lines {
		if strings.Contains(hostapi.SegsText(row), "read f6") {
			mid = i
		}
	}
	if mid < 0 || chat.rows <= chat.viewH {
		t.Fatalf("no mid row to park on: %d, %d rows", mid, chat.rows)
	}
	delete(app.pendingScroll, "chat")
	chat.top, chat.focus, chat.follow = max(mid-(chat.viewH-1)/2, 0), 0, false
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	top := &app.overlays[len(app.overlays)-1]
	if got := top.items[top.visible[top.selected]].label; !strings.Contains(got, "read f6") && !strings.Contains(got, "read f7") {
		t.Fatalf("opened on %q, want the call nearest the view's middle", got)
	}
}

// TestMessagesThinkingTail: the thinking rows the turn draws above its live
// rail name no row the reader can be at, so the tail place stays the last
// settled call, and the band still covers the thinking rows as part of the
// run's drawing.
func TestMessagesThinkingTail(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 100, 30)
	reloadUI(t, host)
	host.AppendMessage("user", "think then hold")
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "f1.go", Label: "read f1", Status: "ok"})
	host.AppendChat(hostapi.ChatMessage{Role: "assistant", Kind: "tool", Tool: "read", Text: "f2.go", Label: "read f2", Status: "ok"})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Thinking: true, Reasoning: "what next", Tools: []hostapi.StreamTool{
		{ID: "c3", Name: "shell", Input: "sleep 25", Label: "hold the turn", Status: "run"},
	}})
	app.renderSync(t)
	app.Frame()
	if got := app.chatRow(); got != 3 {
		t.Fatalf("the tail place at the thinking window is %d, want the last settled call", got)
	}
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	app.Frame()
	top := &app.overlays[len(app.overlays)-1]
	if got := top.items[top.visible[top.selected]].label; got != "read f2" {
		t.Fatalf("opened on %q, want the call at the reader's place", got)
	}
	// the picked call's rows alone carry the band, the thinking rows above
	// them fade like the rest
	if banded := bandedChatText(t, app); len(banded) != 1 || !strings.Contains(banded[0], "read f2") {
		t.Fatalf("band at the thinking window:\n%s", strings.Join(banded, "\n"))
	}
	chat := app.scrolls["chat"]
	if from, to := spanOf(chat.lines, top.selectedAnchor(), true); from < 0 || from != to {
		t.Fatalf("the pick's span %d..%d, want the one row the call draws", from, to)
	}
}

// TestRailBandCoversTheLiveOnlyRun: a text tail leaves the turn's live calls
// a run of their own, anchored by the first call's -1; walking onto its run
// row bands the live rail and keeps the view on it - never a jump to the
// conversation's top, and the pick's rows stay lit under the modal fade.
func TestRailBandCoversTheLiveOnlyRun(t *testing.T) {
	t.Parallel()
	app, host := bootAgentApp(t, 150, 30)
	reloadUI(t, host)
	host.AppendMessage("user", "filler")
	appendReply(host, strings.Repeat("filler. ", 80), hostapi.TurnStat{})
	host.AppendMessage("user", "more filler")
	appendReply(host, strings.Repeat("padding. ", 80), hostapi.TurnStat{})
	host.AppendMessage("user", "even more filler")
	appendReply(host, strings.Repeat("late filler. ", 80), hostapi.TurnStat{})
	host.AppendMessage("user", "the interleaved turn")
	appendReply(host, "some text", hostapi.TurnStat{})
	host.SeedStream(hostapi.StreamSnapshot{Active: true, Tools: []hostapi.StreamTool{
		{ID: "c1", Name: "shell", Input: "sleep 25", Label: "hold the turn", Status: "run"},
	}})
	app.renderSync(t)
	app.Frame()
	if err := host.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	for app.Pump() {
	}
	walkToList(t, app, "step")
	app.Frame()
	chat := app.scrolls["chat"]
	if chat.top == 0 {
		t.Fatal("the view yanked to the top")
	}
	banded := bandedChatText(t, app)
	if !slices.ContainsFunc(banded, func(r string) bool { return strings.Contains(r, "⠋ hold") }) {
		t.Fatalf("live-only band:\n%s", strings.Join(banded, "\n"))
	}
	from, to := spanOf(chat.lines, -1, false)
	if from < 0 || from < chat.top || to >= chat.top+chat.viewH {
		t.Fatalf("the pick is off screen: rows %d..%d of view %d..%d", from, to, chat.top, chat.top+chat.viewH)
	}
	// l on the run row opens the rail level, and l on its live call opens the
	// live view the call's chat row's click opens; esc returns to the rail
	top := func() *overlayState { return &app.overlays[len(app.overlays)-1] }
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	for app.Pump() {
	}
	if id := top().o.ID; id != "messages rail -1" {
		t.Fatalf("l on the live-only run row opened %q", id)
	}
	press(t, app, tea.KeyPressMsg{Code: 'l', Text: "l"})
	waitPrompt(t, app, func() bool { return top().o.ID == "call:-1" })
	for app.Pump() {
	}
	if !strings.Contains(ansi.Strip(app.Frame()), "hold the turn") {
		t.Fatalf("the live view shows nothing:\n%s", ansi.Strip(app.Frame()))
	}
	press(t, app, tea.KeyPressMsg{Code: tea.KeyEscape})
	for app.Pump() {
	}
	if n, id := len(app.overlays), top().o.ID; n != 2 || id != "messages rail -1" {
		t.Fatalf("esc on the live view: %d overlays, %q", n, id)
	}
}
