package hostapi

import (
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestMessagesList: /messages lists every turn's rows in order, each turn a
// section header naming its clock and what it held; a prompt row names the
// message r rewinds to, a run folds into the row l opens the rail from, and
// a call copies the input it acted on.
func TestMessagesList(t *testing.T) {
	t.Parallel()
	sent := time.Date(2026, 10, 19, 14, 2, 0, 0, time.Local)
	tr := []ChatMessage{
		{Role: fantasy.MessageRoleUser, Text: "ping", ID: "m1", SentAt: sent.UnixMilli()},
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
	o := h.messagesList(tr, 0)
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
	if sel := h.messagesList(tr, 4).Selected; sel != 2 {
		t.Errorf("the reader at row 4 selects %d, want the reply", sel)
	}
	if sel := h.messagesList(tr, 3).Selected; sel != 1 {
		t.Errorf("the reader in the run selects %d, want the digest", sel)
	}
	// a prompt names the message r rewinds to
	if prompt := o.Items[0]; prompt.ID != "m1" || prompt.Keys != nil {
		t.Errorf("prompt = %+v", prompt)
	}
	r := o.Keys[slices.IndexFunc(o.Keys, func(k OverlayKey) bool { return k.Key == "r" })]
	if !r.Danger || !r.Applies(OverlayItem{ID: "m1"}) || r.Applies(OverlayItem{Text: "again"}) {
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
	h.reflectMu.Lock()
	defer h.reflectMu.Unlock()
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
