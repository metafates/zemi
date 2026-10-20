package hostapi

import (
	"testing"
	"time"

	"charm.land/fantasy"
)

// TestMessagesPicker: the picker lists the conversation a section per turn,
// the rows the chat draws as messages, each naming the chat row it stands
// for, and a reply's code blocks hanging under it. Thinking and plain calls
// stay out: they are the activity a turn folds into a digest.
func TestMessagesPicker(t *testing.T) {
	const reply = "Two:\n\n```go\nx := 1\n```\n\nand done"
	sent := time.Date(2026, 10, 19, 14, 2, 0, 0, time.Local)
	tr := []ChatMessage{
		{Role: fantasy.MessageRoleUser, Text: "ping", SentAt: sent.UnixMilli()},
		{Role: fantasy.MessageRoleAssistant, Kind: RowReasoning, Text: "hmm"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "shell", Text: "go test", Label: "run the tests", Status: CallOK},
		{Role: fantasy.MessageRoleAssistant, Text: reply},
		{Role: fantasy.MessageRoleAssistant, Kind: RowNotice, Text: "plugin updated"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowError, Text: "it broke"},
		{Role: fantasy.MessageRoleAssistant, Kind: RowShell, Text: "go vet ./...\nout", Status: CallOK},
		{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Tool: "agent", Label: "find the widget", Run: "a1b2c3d4", Status: CallOK},
		{Role: fantasy.MessageRoleAssistant, Text: "  \n"},
	}
	o := messagesPicker(tr)
	if o.ID != "messages" || o.Title != "Messages" || !o.Follow || o.Anchor != OverlayBottomRight {
		t.Fatalf("picker: %s %q follow=%v anchor=%q", o.ID, o.Title, o.Follow, o.Anchor)
	}
	section := "Turn 1"
	type row struct {
		text, desc, copy, mark, run string
		anchor                      int
	}
	want := []row{
		{"ping", sent.Format(ClockLayout), "ping", "", "", 1},
		{"Two:", "7 lines · 1 code block", reply, "", "", 4},
		{"⎿ go · 1 line", "x := 1", "x := 1\n", "", "", 4},
		{"plugin updated", "", "plugin updated", "note", "", 5},
		{"it broke", "", "it broke", "err", "", 6},
		{"$ go vet ./...", "", "go vet ./...\nout", "", "", 7},
		{"find the widget", "agent run", "find the widget", "", "agents view a1b2c3d4", 8},
	}
	if len(o.Items) != len(want) {
		t.Fatalf("%d rows, want %d: %v", len(o.Items), len(want), o.Items)
	}
	for i, w := range want {
		it := o.Items[i].(map[string]any)
		got := row{
			text: it["text"].(string), desc: str(it["desc"]), copy: str(it["copy"]),
			mark: str(it["mark"]), run: str(it["run"]), anchor: it["anchor"].(int),
		}
		if got != w {
			t.Errorf("row %d = %+v, want %+v", i+1, got, w)
		}
		if it["section"] != section {
			t.Errorf("row %d section = %v, want %q", i+1, it["section"], section)
		}
	}
	// the speaker's kind glyph leads the label, the rows that only copy or
	// open a run stay plain
	if len(o.ItemText) != len(o.Items) {
		t.Fatalf("%d labels for %d rows", len(o.ItemText), len(o.Items))
	}
	for _, c := range []struct {
		i         int
		glyph, hl string
	}{
		{0, "▌ ", "UserMsg"}, // the prompt
		{1, "◆ ", "Dim"},     // the reply
	} {
		segs := o.ItemText[c.i]
		if len(segs) != 2 || segs[0].Text != c.glyph || segs[0].HL != c.hl || segs[1].Text != o.Items[c.i].(map[string]any)["text"] {
			t.Errorf("row %d label = %+v", c.i+1, segs)
		}
	}
	for _, i := range []int{2, 3, 4, 5, 6} {
		if o.ItemText[i] != nil {
			t.Errorf("row %d has a speaker label: %+v", i+1, o.ItemText[i])
		}
	}
}

// str is an optional item string.
func str(v any) string {
	s, _ := v.(string)
	return s
}

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
	first := items[0].(map[string]any)
	last := items[1].(map[string]any)
	if first["text"] != "find the widget" || last["text"] != "it is in the corner" {
		t.Fatalf("rows: %v", items)
	}
	if first["section"] != "Turn 1" || first["anchor"] != 1 || last["anchor"] != 2 {
		t.Fatalf("rows: %v", items)
	}
}
