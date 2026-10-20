package hostapi

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestMessagesTurns: /messages heads one row per turn, the prompt it opens
// on first, and what the turn held after it.
func TestMessagesTurns(t *testing.T) {
	t.Parallel()
	sent := time.Date(2026, 10, 19, 14, 2, 0, 0, time.Local)
	tr := []ChatMessage{
		{Role: fantasy.MessageRoleUser, Text: "ping", ID: "m1", SentAt: sent.UnixMilli()},
		{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: "hmm"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "shell", Text: "go test", Status: CallOK},
		{Role: fantasy.MessageRoleAssistant, Text: "pong"},
		{Role: fantasy.MessageRoleUser, Text: "again", SentAt: sent.UnixMilli()},
		{Role: fantasy.MessageRoleAssistant, Text: "twice"},
	}
	h, _ := newTestHost(t)
	o := h.messagesOverlay(tr)
	if o.ID != "messages" || o.Title != "Messages" || !o.Follow || o.Anchor != OverlayBottomRight {
		t.Fatalf("list: %s %q follow=%v anchor=%q", o.ID, o.Title, o.Follow, o.Anchor)
	}
	if len(o.Items) != 2 {
		t.Fatalf("%d turns: %v", len(o.Items), o.Items)
	}
	first := o.Items[0]
	if first.Text != "ping" || first.Anchor != 1 || first.Run != "messages turn 1" ||
		first.Desc != sent.Format(ClockLayout)+" · 1 reply · 2 steps" || first.ID != "m1" {
		t.Errorf("turn 1 = %+v", first)
	}
	second := o.Items[1]
	if second.Text != "again" || second.Anchor != 5 || second.Run != "messages turn 5" {
		t.Errorf("turn 2 = %+v", second)
	}
	// r is the list's own key, and it acts on the rows that name a prompt
	r := o.Keys[slices.IndexFunc(o.Keys, func(k OverlayKey) bool { return k.Key == "r" })]
	if !r.Danger || !r.Applies(OverlayItem{ID: "m1"}) || r.Applies(OverlayItem{Text: "again"}) {
		t.Errorf("rewind key = %+v", r)
	}
}

// TestMessagesTurnRows: a turn lists its messages in order and each activity
// run as one row, the rail the chat folds, which opens its calls a level
// down; a message row with a choice of things to do opens them the same way.
func TestMessagesTurnRows(t *testing.T) {
	t.Parallel()
	tr := []ChatMessage{
		{Role: fantasy.MessageRoleUser, Text: "ping", ID: "m1"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: "hmm"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "shell", Text: "go test", Label: "run the tests", Status: CallErr, Output: "a\nb"},
		{Role: fantasy.MessageRoleAssistant, Text: "the tests fail"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "plugin updated"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "agent", Label: "find the widget", Run: "a1b2c3d4", Status: CallOK},
		{Role: fantasy.MessageRoleUser, Text: "next turn"},
	}
	h, _ := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	h.tMu.Lock()
	h.session = &Session{Transcript: tr}
	h.tMu.Unlock()
	if err := h.openMessagesTurn(1); err != nil {
		t.Fatal(err)
	}
	o := ui.opened[len(ui.opened)-1]
	if o.ID != "messages turn 1" || o.Title != "Messages / Turn 1" || !o.Follow {
		t.Fatalf("turn view: %s %q", o.ID, o.Title)
	}
	type row struct {
		text, mark string
		anchor     int
	}
	var got []row
	for _, it := range o.Items {
		got = append(got, row{it.Text, string(it.Mark), it.Anchor})
	}
	want := []row{
		{"ping", "", 1},
		{"2 steps", "", 2},
		{"the tests fail", "", 4},
		{"plugin updated", "note", 5},
		{"1 step", "", 6},
	}
	if len(got) != len(want) {
		t.Fatalf("rows = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
	// every row's keys act on it from the list; a prompt names the row r
	// rewinds to
	keys := func(it OverlayItem) []string {
		var runs []string
		for _, k := range it.Keys {
			runs = append(runs, k.Run)
		}
		return runs
	}
	prompt := o.Items[0]
	if prompt.ID != "m1" || !slices.Equal(keys(prompt), []string{"messages view 1"}) {
		t.Errorf("prompt = %+v", prompt)
	}
	if reply := o.Items[2]; !slices.Equal(keys(reply), []string{"messages view 4"}) {
		t.Errorf("reply = %+v", reply)
	}
	// a run reads the same whatever it did, and its row opens the rail alone
	rail := o.Items[1]
	if rail.Mark != "" || rail.Desc != "" || rail.Run != "messages rail 2" || rail.Verb != "open" || rail.Keys != nil {
		t.Errorf("rail = %+v", rail)
	}
	// the next turn's prompt is no row of this one
	if strings.Contains(fmt.Sprint(o.Items), "next turn") {
		t.Error("the turn reached past its own rows")
	}

	// the rail's own list walks its calls, and the chat draws that run whole
	if err := h.openMessagesRail(2); err != nil {
		t.Fatal(err)
	}
	r := ui.opened[len(ui.opened)-1]
	if r.ID != "messages rail 2" || r.Title != "Messages / Turn 1 activity" || r.Rail != 2 || !r.Follow {
		t.Fatalf("rail view: %s %q rail=%d", r.ID, r.Title, r.Rail)
	}
	var calls []row
	for _, it := range r.Items {
		calls = append(calls, row{it.Text, string(it.Mark), it.Anchor})
	}
	if !slices.Equal(calls, []row{{"hmm", "", 2}, {"run the tests", "err", 3}}) {
		t.Errorf("calls = %+v", calls)
	}
	if got := keys(r.Items[1]); !slices.Equal(got, []string{"call 3"}) {
		t.Errorf("call keys = %v", got)
	}
	// a call that started a job opens it from its view, never by a second o
	if row := messageCallRow(3, ChatMessage{Kind: RowTool, Tool: "shell", Text: "sleep 9", Job: "job-1"}); len(row.Keys) != 1 || row.Keys[0].Run != "call 3" {
		t.Errorf("a call with a job: %+v", row.Keys)
	}
	if err := h.openMessagesRail(9); err == nil {
		t.Error("a row that opens no rail opened one")
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
	if o := ui.opened[len(ui.opened)-1]; o.Title != "Prompt" || o.BodyMD {
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
	if len(items) != 1 {
		t.Fatalf("%d turns: %v", len(items), items)
	}
	if first := items[0]; first.Text != "find the widget" || first.Anchor != 1 {
		t.Fatalf("turn = %+v", first)
	}
}
