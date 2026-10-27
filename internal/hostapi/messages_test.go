package hostapi

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/tools"
)

// rowKey is an item's identity across rebuilds, what the ui keeps a
// selection by: its id, else the transcript row it names, else its text.
func rowKey(it OverlayItem) string {
	switch {
	case it.ID != "":
		return it.ID
	case it.Anchor != 0:
		return "row " + strconv.Itoa(it.Anchor)
	}
	return it.Text
}

// TestMessagesList: /messages lists every turn's rows in order, each turn a
// section header naming its clock and what it held; a prompt row names the
// message r rewinds to, a run folds into the row l opens the rail from, and
// a call copies the input it acted on.
func TestMessagesList(t *testing.T) {
	t.Parallel()
	sent := time.Date(2026, 10, 19, 14, 2, 0, 0, time.Local)
	tr := []ChatMessage{
		{Role: fantasy.MessageRoleUser, Text: "ping", Seq: 7, SentAt: sent.UnixMilli()},
		{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: "hmm"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "shell", Text: "go test", Label: "run the tests", Status: CallErr, Output: "a\nb"},
		{Role: fantasy.MessageRoleAssistant, Text: "the tests fail"},
		{Role: fantasy.MessageRoleUser, Text: "again", SentAt: sent.UnixMilli()},
		{Role: fantasy.MessageRoleAssistant, Text: "twice"},
	}
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.tMu.Lock()
	h.session = &Session{Transcript: tr}
	h.tMu.Unlock()
	o := h.messagesList(pickTr{tr: tr}, 0)
	if o.ID != "messages" || o.Title != "Messages" || !o.Follow || o.Anchor != OverlayBottomRight {
		t.Fatalf("list: %s %q follow=%v anchor=%q", o.ID, o.Title, o.Follow, o.Anchor)
	}
	clock := sent.Format(ClockLayout)
	type row struct {
		text, section, mark, run string
		anchor                   int
	}
	var got []row
	for _, it := range o.Items {
		got = append(got, row{it.Text, it.Section, string(it.Mark), it.Run, it.Anchor})
	}
	turn1, turn2 := "Turn 1 · "+clock+" · 1 reply · 2 steps", "Turn 2 · "+clock+" · 1 reply"
	want := []row{
		{"ping", turn1, "", "messages view 1", 1},
		{"2 steps", turn1, "", "messages rail 2", 2},
		{"the tests fail", turn1, "", "messages view 4", 4},
		{"again", turn2, "", "messages view 5", 5},
		{"twice", turn2, "", "messages view 6", 6},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
	// the reader's row names the element holding it: a message row by its
	// transcript row, a row inside a run by the run's digest
	if sel := h.messagesList(pickTr{tr: tr}, 4).Selected; sel != 2 {
		t.Errorf("the reader at row 4 selects %d, want the reply", sel)
	}
	if sel := h.messagesList(pickTr{tr: tr}, 3).Selected; sel != 1 {
		t.Errorf("the reader in the run selects %d, want the digest", sel)
	}
	// a prompt names the message r rewinds to
	if prompt := o.Items[0]; prompt.ID != "7" || prompt.Keys != nil {
		t.Errorf("prompt = %+v", prompt)
	}
	r := o.Keys[slices.IndexFunc(o.Keys, func(k OverlayKey) bool { return k.Key == "r" })]
	if !r.Danger || !r.Applies(OverlayItem{ID: "7"}) || r.Applies(OverlayItem{Text: "again"}) {
		t.Errorf("rewind key = %+v", r)
	}
	// a call copies the input it acted on; one that took nothing worth
	// keeping copies nothing
	if call := messageCallRow(3, ChatMessage{Kind: RowTool, Tool: "shell", Text: "go test"}); call.Copy != "go test" {
		t.Errorf("a shell call copies %q", call.Copy)
	}
	if call := messageCallRow(4, ChatMessage{Kind: RowReasoning, Text: "thinking it over"}); call.Copy != "" {
		t.Errorf("a thought copies %q", call.Copy)
	}

	// the rail's own list walks its calls, and the chat draws that run whole
	if err := h.openMessagesRail(2); err != nil {
		t.Fatal(err)
	}
	ro := ui.opened[len(ui.opened)-1]
	if ro.ID != "messages rail 2" || ro.Title != "Messages / Turn 1 activity" || ro.Rail != 2 || !ro.Follow {
		t.Fatalf("rail view: %s %q rail=%d", ro.ID, ro.Title, ro.Rail)
	}
	if call := ro.Items[1]; call.Run != "call 3" || call.Verb != VerbGo || call.Keys != nil {
		t.Errorf("call = %+v", call)
	}
	if err := h.openMessagesRail(9); err == nil {
		t.Error("a row that opens no rail opened one")
	}

	// /messages opens the list on the reader's row, with the rail of the run
	// holding it under the list
	ui.opened = nil
	ui.chatRow = 3
	if err := h.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	if len(ui.opened) != 2 {
		t.Fatalf("%d overlays opened: %v", len(ui.opened), ui.opened)
	}
	if list, rail := ui.opened[0], ui.opened[1]; list.ID != "messages" || list.Selected != 1 ||
		rail.ID != "messages rail 2" || rail.Selected != 1 {
		t.Fatalf("open at the run: %+v, %+v", ui.opened[0], ui.opened[1])
	}
}

// TestMessagesView: reading a row whole shows a reply as its markdown and a
// prompt as the text it is.
func TestMessagesView(t *testing.T) {
	t.Parallel()
	tr := []ChatMessage{
		{Role: fantasy.MessageRoleUser, Text: "ping"},
		{Role: fantasy.MessageRoleAssistant, Text: "# a heading\n\nbody"},
	}
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.tMu.Lock()
	h.session = &Session{Transcript: tr}
	h.tMu.Unlock()
	if err := h.openMessageView(2); err != nil {
		t.Fatal(err)
	}
	o := ui.opened[len(ui.opened)-1]
	if o.Title != "Reply" || !o.BodyMD || o.Body != "# a heading\n\nbody" {
		t.Errorf("reply view = %q %v %q", o.Title, o.BodyMD, o.Body)
	}
	if err := h.openMessageView(1); err != nil {
		t.Fatal(err)
	}
	// a prompt reads in markdown too: the model's own text is written in it
	if o := ui.opened[len(ui.opened)-1]; o.Title != "Prompt" || !o.BodyMD {
		t.Errorf("prompt view = %q %v", o.Title, o.BodyMD)
	}
	if err := h.openMessageView(9); err == nil {
		t.Error("row 9 exists")
	}
}

// str is an optional item string.
// TestMessagesRun: with a run on screen, the picker lists that run's rows,
// not the session's, and it is what a bare /copy opens.
func TestMessagesRun(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "pong")
	if err := h.AgentSend("ping"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	run := "a1b2c3d4"
	h.DB.CreateSession(run, h.AgentSessionID(), "look around", "explore")
	addText(t, h, run, "user", "find the widget")
	addText(t, h, run, "assistant", "it is in the corner")
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.ViewAgent(run); err != nil {
		t.Fatal(err)
	}
	if err := h.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	if len(ui.opened) != 1 {
		t.Fatalf("%d overlays opened", len(ui.opened))
	}
	items := ui.opened[0].Items
	if len(items) != 2 {
		t.Fatalf("%d rows: %v", len(items), items)
	}
	if first := items[0]; first.Text != "find the widget" || first.Anchor != 1 {
		t.Fatalf("prompt = %+v", first)
	}
}

// TestMessagesLiveCalls: the picker reads the conversation the chat draws -
// the calls a running turn has not settled yet are rows of the run its
// settled rows end in, listed call by call in the rail level and counted by
// the list's run row; a still running one opens its live view, the one its
// chat row's click opens. Nothing lists while no call has settled (the run
// does not exist yet), and a reply that started folds the run the way the
// chat does.
func TestMessagesLiveCalls(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, h.RT, `zemi.ui.widget.add({ id = "railprobe", region = "header", render = function(ctx)
		return { { { text = tostring(ctx.rail) } } }
	end })`)
	h.AppendMessage("user", "watch the dev server")
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "shell", Text: "npm run dev", Label: "start the dev server", Status: CallBG, Job: "job-1", Output: "started job-1"})
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "job_read", Text: "job-1", Status: CallOK, Output: "job-1: running"})
	seed := func(tools ...StreamTool) {
		h.SeedStream(StreamSnapshot{Active: true, Tools: tools})
	}
	live := []StreamTool{
		{ID: "c3", Name: "job_read", Input: "job-1", Status: CallRun},
		{ID: "c4", Name: "edit", Input: "f1.go", Label: "fix f1", Status: CallOK},
	}
	seed(live...)

	o := h.messagesList(h.pickerTranscript(), 0)
	run := o.Items[1]
	if run.Text != "4 steps" || run.Anchor != 2 || run.Run != "messages rail 2" {
		t.Fatalf("run row = %+v, want the live calls counted", run)
	}

	ro, ok := h.messagesRailOverlay(h.pickerTranscript(), 2, 0)
	if !ok {
		t.Fatal("no rail view")
	}
	type row struct {
		label  string
		anchor int
		run    string
	}
	var got []row
	for _, it := range ro.Items {
		got = append(got, row{it.Text, it.Anchor, it.Run})
	}
	want := []row{
		{"start the dev server", 2, "call 2"},
		{"job_read job-1", 3, "call 3"},
		{"job_read job-1", -1, "call -1"},
		{"fix f1", -2, "call -2"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rail rows = %+v, want %+v", got, want)
	}
	// the live edit is a row the reader can pick: its mark says it is done,
	// and y copies the input the call acted on
	edit := ro.Items[3]
	if edit.Mark != tools.MarkOK || edit.Copy != "f1.go" {
		t.Errorf("live edit = %+v", edit)
	}
	if call := messageCallRow(-2, pickTr{live: []ChatMessage{{Kind: RowTool, Tool: "read", Text: "a.go"}}}.at(-2)); call.Run != "call -2" || call.Verb != VerbGo {
		t.Errorf("a live call opens %+v", call.Run)
	}

	// /messages with the reader in the run opens the rail on their row
	ui.chatRow = 3
	if err := h.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	if list, rail := ui.opened[0], ui.opened[1]; list.Selected != 1 || rail.ID != "messages rail 2" || rail.Selected != 1 {
		t.Fatalf("open at the run: %+v, %+v", list, rail)
	}

	// a prompt the turn streams under is no run the calls extend: the calls
	// hold a run of their own, named by the first call's -1, until one settles
	h.AppendMessage("user", "next")
	o = h.messagesList(h.pickerTranscript(), 0)
	run = o.Items[3]
	if run.Text != "2 steps" || run.Anchor != -1 || run.Run != "messages rail -1" {
		t.Fatalf("live-only run row = %+v", run)
	}
	ro, ok = h.messagesRailOverlay(h.pickerTranscript(), -1, 0)
	if !ok || len(ro.Items) != 2 || ro.Items[0].Anchor != -1 || ro.Items[1].Anchor != -2 {
		t.Fatalf("live-only rail = %+v ok=%v", ro.Items, ok)
	}

	// the chat draws the run the picker's rail opens whole: the negative
	// anchor reaches the ctx, and with more live calls than a rail keeps
	// none stands behind an earlier row - the walk bands a row per call
	calls := []StreamTool{
		{ID: "c3", Name: "job_read", Input: "job-1", Label: "step 3", Status: CallRun},
		{ID: "c4", Name: "edit", Input: "f1.go", Label: "step 4", Status: CallOK},
		{ID: "c5", Name: "read", Input: "a.go", Label: "step 5", Status: CallOK},
		{ID: "c6", Name: "read", Input: "b.go", Label: "step 6", Status: CallOK},
		{ID: "c7", Name: "read", Input: "c.go", Label: "step 7", Status: CallOK},
		{ID: "c8", Name: "read", Input: "d.go", Label: "step 8", Status: CallOK},
		{ID: "c9", Name: "read", Input: "e.go", Label: "step 9", Status: CallOK},
		{ID: "c10", Name: "read", Input: "f.go", Label: "step 10", Status: CallOK},
	}
	seed(calls...)
	ro, ok = h.messagesRailOverlay(h.pickerTranscript(), -1, 0)
	if !ok || ro.Rail != -1 {
		t.Fatalf("live-only rail = %+v ok=%v", ro, ok)
	}
	if lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("railprobe"), 20, 1, UIState{Rail: ro.Rail}); err != nil {
		t.Fatal(err)
	} else if got := SegsText(lines[0]); got != "-1" {
		t.Fatalf("the picker's rail reads %q in the ctx, want -1", got)
	}
	full := widgetText(t, h, "chat", 98, UIState{Rail: ro.Rail})
	if strings.Contains(full, "earlier") {
		t.Error("the live-only run drew folded behind an earlier row")
	}
	for i := 3; i <= 10; i++ {
		if !strings.Contains(full, "step "+strconv.Itoa(i)) {
			t.Errorf("step %d drew no row", i)
		}
	}
	if open := widgetText(t, h, "chat", 98, UIState{}); !strings.Contains(open, "earlier") {
		t.Error("the live rail kept every call with no rail open")
	}

	// the turn idle, the calls settle into the transcript and turn 2 folds
	// them into a run row of its own
	seed(
		StreamTool{ID: "c3", Name: "job_read", Input: "job-1", Status: CallOK},
		StreamTool{ID: "c4", Name: "edit", Input: "f1.go", Label: "fix f1", Status: CallOK},
	)
	h.flushOutput(TurnStat{})
	o = h.messagesList(h.pickerTranscript(), 0)
	if run := o.Items[3]; run.Text != "2 steps" || run.Anchor != 5 {
		t.Fatalf("settled run row = %+v", run)
	}
}

// TestRefreshMessagesKeepsNewLevels: a level tracked while a rebuild runs
// stays tracked, so the flush that lands after it reaches it; a level the
// conversation dropped leaves the store.
func TestRefreshMessagesKeepsNewLevels(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.AppendMessage("user", "one")
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "read", Text: "a.go", Status: CallOK})
	h.trackMessages("messages")
	// the refresh rebuilds the stale levels; the rail level of the run that
	// just appeared is tracked by the rebuild's own refresh callback, the way
	// an open landing mid-rebuild would
	ui.onRefresh = func(string) { h.trackMessages("messages rail 2") }
	h.refreshMessagesFrom(h.pickerTranscript())
	if !slices.Contains(h.msgLevels, "messages rail 2") {
		t.Fatalf("levels = %v, the level tracked mid-rebuild is gone", h.msgLevels)
	}
	// a level the conversation dropped leaves the store
	h.refreshMessagesFrom(pickTr{tr: nil})
	if slices.Contains(h.msgLevels, "messages rail 2") {
		t.Fatalf("levels = %v, the dropped level stayed", h.msgLevels)
	}
}

// TestMessagesPageUp: a page-up past the picker's first row loads the page of
// log rows before the window and extends the list with it; the window's rows
// keep their anchors and identities (the chat's bands and jumps read them),
// the loaded rows name no transcript row, and nothing loads past the log's
// first message.
func TestMessagesPageUp(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	for i := range 1100 {
		if _, err := h.DB.AppendLog(kindUser, fmt.Sprintf("message %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	// the window opened on the log's tail at boot, when the log was empty:
	// the rows land and the chat opens on their tail the way a full log does
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows))
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	before := ui.opened[len(ui.opened)-1]
	if len(before.Items) != chatWindowRows {
		t.Fatalf("the window lists %d rows", len(before.Items))
	}
	if before.Items[0].Anchor != 1 {
		t.Fatalf("the window's first row anchors %d", before.Items[0].Anchor)
	}
	o, ok := h.messagesOlder()
	if !ok {
		t.Fatal("nothing loaded for a window that opened mid-log")
	}
	// the page holds the 100 rows before the window's first: seqs 0..99
	if len(o.Items) != chatWindowRows+100 {
		t.Fatalf("the extended list holds %d rows", len(o.Items))
	}
	for i := 100; i < len(o.Items); i++ {
		was := before.Items[i-100]
		if o.Items[i].Anchor != was.Anchor || rowKey(o.Items[i]) != rowKey(was) {
			t.Fatalf("window row %d moved: anchor %d key %q, was %d %q",
				i, o.Items[i].Anchor, rowKey(o.Items[i]), was.Anchor, rowKey(was))
		}
	}
	if o.Items[0].Anchor != 0 || o.Items[0].ID != "" {
		t.Errorf("a loaded row anchors %d id %q, want none", o.Items[0].Anchor, o.Items[0].ID)
	}
	if o.Items[1].ID != "1" || o.Items[1].Run != "messages view #1" {
		t.Errorf("the loaded prompt = id %q run %q", o.Items[1].ID, o.Items[1].Run)
	}
	if _, ok := h.messagesOlder(); ok {
		t.Error("a page loaded past the log's first message")
	}
	// a rewind's swap drops the loaded pages with the rows they name
	h.SetTranscript(h.Session().Transcript)
	h.msgMu.Lock()
	n := len(h.msgOld)
	h.msgMu.Unlock()
	if n != 0 {
		t.Errorf("%d loaded rows survived the swap", n)
	}
}
