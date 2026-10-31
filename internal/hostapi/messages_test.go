package hostapi

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
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
// section header naming its clock and what it held; a run folds into the row
// l opens the rail from, and a call copies the input it acted on.
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
	// both turns ran the same day, so the date prefixes the first section only
	day := sent.Format("Jan 02")
	type row struct {
		text, section, mark, run string
		anchor                   int
	}
	var got []row
	for _, it := range o.Items {
		got = append(got, row{it.Text, it.Section, string(it.Mark), it.Run, it.Anchor})
	}
	turn1, turn2 := "Turn 1 · "+day+" · "+clock+" · 1 reply", "Turn 2 · "+clock+" · 1 reply"
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
	// a prompt row's run reads it whole, and no key acts on it
	if prompt := o.Items[0]; prompt.ID != "" || prompt.Keys != nil {
		t.Errorf("prompt = %+v", prompt)
	}
	if slices.ContainsFunc(o.Keys, func(k OverlayKey) bool { return k.Key == "r" }) {
		t.Errorf("the list offers a rewind key")
	} // a call copies the input it acted on; one that took nothing worth
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

// TestStampingPairsResultRows: a tool result's log row draws no row of its
// own, so the pairing passes it instead of spending the next row's slot on
// it - which stalled every turn after the first tool call and left its rows
// unnamed, the log ids on them dead.
func TestStampingPairsResultRows(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	turn := func(text string) {
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: text})
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: tools.ToolName("read"), Text: "a.go", Label: "read a", Status: CallOK})
		h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "all done " + text})
		for _, row := range []struct {
			kind       store.LogKind
			text, meta string
		}{
			{store.KindUser, text, ""},
			{store.KindTool, `{"path":"a.go"}`, `{"tool":"read"}`},
			{store.KindEcho, "the file body", `{"tool":"read","ok":true}`},
			{store.KindAgent, "all done " + text, ""},
		} {
			if _, err := h.DB.AppendLog(row.kind, row.text, row.meta, "", ""); err != nil {
				t.Fatal(err)
			}
		}
		h.stampLogSeqs()
	}
	turn("one")
	turn("two")
	var got []int64
	for _, m := range h.Session().Transcript {
		got = append(got, m.Seq)
	}
	if want := []int64{0, 1, 3, 4, 5, 7}; !slices.Equal(got, want) {
		t.Fatalf("stamped seqs = %v, want %v", got, want)
	}
	// the picker's prompt rows anchor at the rows the seqs stamp
	o := h.messagesList(h.pickerTranscript(), 0)
	if o.Items[0].Anchor != 1 || o.Items[3].Anchor != 4 {
		t.Fatalf("prompt anchors = %d %d, want 1 and 4", o.Items[0].Anchor, o.Items[3].Anchor)
	}
}

// TestMessagesCrossWindow: the turns another window logged appear in the
// picker, the way /memories shows them, under their own section and named by
// their log seq; the rows this window drew but has not stamped yet stay out.
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
	replyStub(t, h, "pong")
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
		if _, err := h.DB.AppendLog(store.KindUser, fmt.Sprintf("message %d", i), "", "", ""); err != nil {
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
	// the log's first message is seq 0, a real row: its view names its seq
	// like every other one
	if o.Items[0].Anchor != 0 {
		t.Errorf("the first logged prompt anchors %d, want 0", o.Items[0].Anchor)
	}
	if o.Items[1].Run != "messages view #1" {
		t.Errorf("the loaded prompt's run = %q", o.Items[1].Run)
	}
	// enter opens the view like l does: the row names no chat place to close on
	if o.Items[1].Verb != VerbOpen {
		t.Errorf("the loaded prompt's enter = %q, want the view opened", o.Items[1].Verb)
	}
	if _, ok := h.messagesOlder(); ok {
		t.Error("a page loaded past the log's first message")
	}
	// a reset's swap drops the loaded pages with the rows they name
	h.SetTranscript(h.Session().Transcript)
	h.msgMu.Lock()
	n := len(h.msgOld)
	h.msgMu.Unlock()
	if n != 0 {
		t.Errorf("%d loaded rows survived the swap", n)
	}
}

// TestMessagesEarlierFoldsRails: the loaded pages of log rows list the way
// the window's turns do - a run folded into the rail its row opens, a call
// opening the view its chat row opens, read from the log by seq.
func TestMessagesEarlierFoldsRails(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	turn := func(i int) {
		for _, row := range []struct {
			kind       store.LogKind
			text, meta string
		}{
			{store.KindUser, fmt.Sprintf("older turn %d: fix the widget", i), ""},
			{store.KindTool, `{"command":"echo hi","about":"say hi"}`, `{"tool":"shell","id":"c` + strconv.Itoa(i) + `"}`},
			{store.KindEcho, "hi", `{"tool":"shell","ok":true,"id":"c` + strconv.Itoa(i) + `","status":"ok"}`},
			{store.KindThought, "thinking it over", `{"ms":250}`},
			{store.KindAgent, "done.", ""},
		} {
			if _, err := h.DB.AppendLog(row.kind, row.text, row.meta, "", ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range 50 {
		turn(i)
	}
	for i := range 1000 { // the window: 1000 filler rows the pages stop before
		if _, err := h.DB.AppendLog(store.KindUser, fmt.Sprintf("window row %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.DB.LogTail(chatWindowRows)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows))
	o, ok := h.messagesOlder()
	if !ok {
		t.Fatal("nothing loaded")
	}
	var rail, call OverlayItem
	for _, it := range o.Items {
		switch {
		case it.Text == "2 steps" && rail.Run == "":
			rail = it
		case it.Mark == tools.MarkSpin || strings.Contains(it.Text, "say hi"):
		}
		if it.Text == "older turn 0: fix the widget" {
			call = it // the prompt of the oldest turn, the page's first element
		}
	}
	if rail.Run != "messages rail #1" || rail.Verb != VerbOpen || rail.Anchor != 0 {
		t.Fatalf("the folded run = %+v, want the seq rail opened", rail)
	}
	if call.Run != "messages view #0" || call.Verb != VerbOpen {
		t.Fatalf("the loaded prompt = %+v, want its log row's view", call)
	}
	// the rail reads the logged run call by call, each row its own view
	if _, ok := h.messagesRailOverlaySeq(1); !ok {
		t.Fatal("the logged run opened no rail")
	}
	// the rail's rows walk the logged calls, each opening its own view
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openMessagesRailSeq(1); err != nil {
		t.Fatal(err)
	}
	ro := ui.opened[len(ui.opened)-1]
	if ro.ID != "messages rail #1" || ro.Title != "Messages / earlier activity" || len(ro.Items) != 2 {
		t.Fatalf("rail: %s %q, %d rows", ro.ID, ro.Title, len(ro.Items))
	}
	if call := ro.Items[0]; call.Run != "call #1" || call.Text != "say hi" {
		t.Fatalf("the rail's call = %+v", call)
	}
	if thought := ro.Items[1]; thought.Run != "call #3" {
		t.Fatalf("the rail's thought = %+v", thought)
	}
	if err := h.openLogCall(1); err != nil {
		t.Fatal(err)
	}
	cv := ui.opened[len(ui.opened)-1]
	if cv.ID != "call:#1" || cv.Title != "Command" || len(cv.Fields) != 2 ||
		cv.Fields[0].Value != "ok" || cv.Fields[1].Value != "say hi" {
		t.Fatalf("call view: %s %q fields %+v", cv.ID, cv.Title, cv.Fields)
	}
	if cv.Prev != "" || cv.Next != "call #3" {
		t.Fatalf("the logged call steps %q -> %q", cv.Prev, cv.Next)
	}
	if !strings.Contains(cv.Body, "echo hi") || !strings.Contains(cv.Body, "hi") {
		t.Fatalf("the view body reads %q", cv.Body)
	}
	// a thought's view carries the time its row logged
	if err := h.openLogCall(3); err != nil {
		t.Fatal(err)
	}
	tv := ui.opened[len(ui.opened)-1]
	if tv.Title != "Thought" || tv.Fields[1].Name != "time" || tv.Fields[1].Value != "0.2s" {
		t.Fatalf("the logged thought = %q %+v", tv.Title, tv.Fields)
	}
	// the view a logged call opens is the one the chat's own rows open: an
	// echo of a live turn's shell call reads the same shape
	if err := h.openCall(1); err == nil {
		t.Error("row 1 of the window is no call")
	}
}

// TestLogRowsPairsParallelCalls: two calls of one tool log their rows apart
// and their echoes after them; each call reads its own echo, by the call id
// both metas carry, not its neighbour's.
func TestLogRowsPairsParallelCalls(t *testing.T) {
	t.Parallel()
	tool := func(seq int64, id string) store.LogEntry {
		return store.LogEntry{Seq: seq, Kind: store.KindTool, Text: `{"command":"ls"}`, MetaJSON: `{"tool":"shell","id":"` + id + `"}`}
	}
	echo := func(seq int64, id, out string) store.LogEntry {
		return store.LogEntry{Seq: seq, Kind: store.KindEcho, Text: out, MetaJSON: `{"tool":"shell","ok":true,"id":"` + id + `"}`}
	}
	user := func(seq int64) store.LogEntry { return store.LogEntry{Seq: seq, Kind: store.KindUser, Text: "next"} }
	tr := logRows([]store.LogEntry{
		tool(0, "a"), tool(1, "b"), echo(2, "b", "second"), echo(3, "a", "first"), user(4),
	})
	if len(tr) != 3 || tr[0].Output != "first" || tr[1].Output != "second" {
		t.Fatalf("logRows drew %+v, want each call its own echo", tr)
	}
	// rows the ids never reached (an older store) pair by adjacency, the
	// order results land in
	legacy := func(seq int64) store.LogEntry {
		return store.LogEntry{Seq: seq, Kind: store.KindTool, Text: `{"command":"ls"}`, MetaJSON: `{"tool":"shell"}`}
	}
	legacyEcho := func(seq int64, out string) store.LogEntry {
		return store.LogEntry{Seq: seq, Kind: store.KindEcho, Text: out, MetaJSON: `{"tool":"shell","ok":true}`}
	}
	tr = logRows([]store.LogEntry{legacy(0), legacy(1), legacyEcho(2, "one"), legacyEcho(3, "two"), user(4)})
	if len(tr) != 3 || tr[0].Output != "one" || tr[1].Output != "two" {
		t.Fatalf("legacy rows drew %+v", tr)
	}
	// one id on every call, the shape a fake model logs: each call still
	// reads its own echo, the oldest past it, in log order
	tr = logRows([]store.LogEntry{
		tool(0, "t1"), echo(1, "t1", "one"), tool(2, "t1"), echo(3, "t1", "two"), user(4),
	})
	if len(tr) != 3 || tr[0].Output != "one" || tr[1].Output != "two" {
		t.Fatalf("repeated ids drew %+v, want each call its own echo", tr)
	}
}

// TestStampPairsASplitMessage: a message over the chat split logs as sibling
// rows while the transcript holds one; the row names the first chunk and the
// pairing walks past the siblings. Marked gone instead, the cursor stalls on
// the second chunk and the window never pairs again - deaf to live sync.
func TestStampPairsASplitMessage(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	full := strings.Repeat("x", 9000) + "tail"
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: full})
	if _, err := h.DB.AppendLog(store.KindUser, full[:8000], "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.AppendLog(store.KindUser, full[8000:], "", "", ""); err != nil {
		t.Fatal(err)
	}
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Text: "reply"})
	if _, err := h.DB.AppendLog(store.KindAgent, "reply", "", "", ""); err != nil {
		t.Fatal(err)
	}
	h.stampLogSeqs()
	tr := h.Session().Transcript
	if tr[0].Seq != 0 || tr[1].Seq != 2 {
		t.Fatalf("the split message's row names its first chunk and the next row pairs: %d, %d", tr[0].Seq, tr[1].Seq)
	}
	h.tMu.RLock()
	from := h.stampFrom
	h.tMu.RUnlock()
	if from != 3 {
		t.Fatalf("the cursor walked past the siblings: %d", from)
	}
	// the split's row must not read as another window's wipe: a false wipe
	// drops the chat, the goal with it
	h.storeGoal(&Goal{Condition: "the tests pass", SetAt: 1})
	h.tMu.Lock()
	wiped := h.logWipedLocked()
	h.tMu.Unlock()
	if wiped {
		t.Fatal("the split message's row reads as another window's wipe")
	}
	if _, err := h.DB.GetKV(goalKV); err != nil {
		t.Fatalf("the goal survives the split: %v", err)
	}
}

// TestLogRowsBrowseKeepsUnpairedEchoes: the transcript folds an attachment's
// echo and a !command's full output into the row they belong to; the
// browser, where the log's whole truth lives, draws what nothing consumed.
// The logged block opens with the newline its writer joined it with - the
// shape that drew blank rows before firstLine skipped it.
func TestLogRowsBrowseKeepsUnpairedEchoes(t *testing.T) {
	t.Parallel()
	rows := []store.LogEntry{
		{Seq: 0, Kind: store.KindEcho, Text: "\n<file path=\"task.txt\">\nwords\n</file>", MetaJSON: `{"tool":"attach","ok":true}`},
		{Seq: 1, Kind: store.KindUser, Text: "read @task.txt"},
	}
	tr := logRows(rows)
	if len(tr) != 1 {
		t.Fatalf("the transcript folds the attach echo: %+v", tr)
	}
	browse := logRowsFor(rows, true)
	if len(browse) != 2 || browse[0].Kind != RowNotice || browse[0].Text != `<file path="task.txt">` || browse[0].Detail != rows[0].Text || browse[1].Seq != 1 {
		t.Fatalf("the browser draws the unpaired echo in place: %+v", browse)
	}
}

// TestMessagesPagesFromBoot: a boot over a log longer than the window fills
// the chat straight from the log, past SetTranscript, so the pager's "no
// pages loaded" has to be the host's starting value and not the field's
// zero; a page-up on the first keypress loads what the window shed.
func TestMessagesPagesFromBoot(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	dirs := config.Dirs{StateHome: t.TempDir(), DataHome: t.TempDir(), ProjectDir: t.TempDir()}
	// the store keys on the project's repo root: a bare temp dir is no repo,
	// and the boot opens it under the evaluated path (macOS /var -> /private/var)
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
	h := NewHost(r, log, config.NewStore(), dirs)
	h.ReleaseBuilder()
	t.Cleanup(h.Close)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	// the dead tail of the seed's last turn draws the boot marker: the
	// window plus the row that says the window closed on it
	if n := len(h.Session().Transcript); n != chatWindowRows+1 {
		t.Fatalf("the boot window holds %d rows", n)
	}
	o, ok := h.messagesOlder()
	if !ok {
		t.Fatal("nothing loaded for a window that booted mid-log")
	}
	if o.Items[0].Section != "Earlier" {
		t.Fatalf("the first loaded row reads %q, want Earlier", o.Items[0].Section)
	}
	// the dead tail's marker row rides the transcript, so the extended list
	// carries it
	if len(o.Items) != chatWindowRows+100+1 {
		t.Fatalf("the extended list holds %d rows", len(o.Items))
	}
	if o.Items[0].Anchor != 0 {
		t.Errorf("the first logged prompt anchors %d, want 0", o.Items[0].Anchor)
	}
}
