// memories_test.go pins /memories: the live view as rows exactly the model
// reads, the zoom round-trip a line's children make, the placeholders unbuilt
// leaves carry, and the jump a leaf's anchor names in the chat.

package hostapi

import (
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// seedView writes a view the browser reads: six messages, the first two
// merged into node(1,0), the next two into node(1,1), the last two leaves,
// the last one unbuilt. The log rows are long, so the builder's leaf builds
// fail (no model) and the view stays as seeded.
func seedView(t *testing.T, h *Host) {
	t.Helper()
	for range 6 {
		if _, err := h.DB.AppendLog(kindUser, strings.Repeat("row ", 200), "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		if _, err := h.DB.InsertNode(0, int64(i), "msg "+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.DB.InsertNode(1, 0, "msgs 0-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.InsertNode(1, 1, "msgs 2-3"); err != nil {
		t.Fatal(err)
	}
	if n, err := h.DB.MergeViewLines([]store.ViewMerge{{L: 1, I: 0}, {L: 1, I: 1}}); err != nil || n != 2 {
		t.Fatalf("the merges: %d applied, %v", n, err)
	}
}

// lines is the rows the overlay lists, in order.
func lines(o Overlay) []string {
	out := make([]string, len(o.Items))
	for i, it := range o.Items {
		out[i] = it.Text
	}
	return out
}

// seedMemories boots a host with the seeded view and a fake screen.
func seedMemories(t *testing.T) (*Host, *fakeUI) {
	t.Helper()
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	seedView(t, h)
	return h, ui
}

func TestMemoriesZoom(t *testing.T) {
	t.Parallel()
	h, _ := seedMemories(t)

	if err := h.MemoriesCommand(""); err != nil {
		t.Fatal(err)
	}
	o := h.memoriesOverlay()
	want := []string{"0+2|msgs 0-1", "2+2|msgs 2-3", "4+1|msg 4", "5+1|(not summarized yet: zoom it)"}
	if got := lines(o); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the view rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if o.Items[0].Run != "memories zoom 1 0" || o.Items[2].Run != "" {
		t.Errorf("run = %q, leaf = %q", o.Items[0].Run, o.Items[2].Run)
	}
	if o.Items[0].Keys[1].Key != "v" || o.Items[0].Keys[1].Run != "memories view 1 0" {
		t.Errorf("v = %+v", o.Items[0].Keys[1])
	}
	if len(o.Items[0].Keys) != 2 {
		t.Errorf("u offered with no zoom open: %+v", o.Items[0].Keys)
	}

	// zoom opens the line's two children in its place, the rest unchanged
	if err := h.MemoriesCommand("zoom 1 0"); err != nil {
		t.Fatal(err)
	}
	o = h.memoriesOverlay()
	zoomed := []string{"0+1|msg 0", "1+1|msg 1", "2+2|msgs 2-3", "4+1|msg 4", "5+1|(not summarized yet: zoom it)"}
	if got := lines(o); strings.Join(got, "\n") != strings.Join(zoomed, "\n") {
		t.Fatalf("zoomed rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(zoomed, "\n"))
	}
	if o.Selected != 0 {
		t.Errorf("the zoom landed on selection %d, want the first child", o.Selected)
	}
	if o.Items[0].Keys[2].Key != "u" || o.Items[0].Keys[2].Run != "memories pop" {
		t.Errorf("u = %+v", o.Items[0].Keys[2])
	}

	// the other merged line zooms beside the open one, and u pops one zoom
	// at a time
	if err := h.MemoriesCommand("zoom 1 1"); err != nil {
		t.Fatal(err)
	}
	if got := lines(h.memoriesOverlay()); strings.Join(got, "|") != "0+1|msg 0|1+1|msg 1|2+1|msg 2|3+1|msg 3|4+1|msg 4|5+1|(not summarized yet: zoom it)" {
		t.Fatalf("both lines open: %v", got)
	}
	if err := h.MemoriesCommand("pop"); err != nil {
		t.Fatal(err)
	}
	if got := lines(h.memoriesOverlay()); strings.Join(got, "|") != "0+1|msg 0|1+1|msg 1|2+2|msgs 2-3|4+1|msg 4|5+1|(not summarized yet: zoom it)" {
		t.Fatalf("after one pop: %v", got)
	}
	if err := h.MemoriesCommand("pop"); err != nil {
		t.Fatal(err)
	}
	if err := h.MemoriesCommand("pop"); err != nil { // popping an empty stack is nothing
		t.Fatal(err)
	}
	o = h.memoriesOverlay()
	if got := lines(o); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("popped back to: %v", got)
	}

	// a leaf has no children to open, and a stranger is no line of the view
	if err := h.MemoriesCommand("zoom 0 4"); err == nil {
		t.Error("zooming a leaf ran")
	}
	if err := h.MemoriesCommand("zoom 3 1"); err == nil {
		t.Error("zooming a node the view does not hold ran")
	}
}

// TestMemoriesJump: a row's anchor is the window row the chat draws the
// line's first covered message at, and a line the window does not reach
// names none.
func TestMemoriesJump(t *testing.T) {
	t.Parallel()
	h, _ := seedMemories(t)
	rows, err := h.DB.LogFrom(0, 6)
	if err != nil {
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows)) // window rows 1..6, seqs 0..5

	o := h.memoriesOverlay()
	for _, c := range []struct {
		line string
		want int
	}{
		{"0+2|msgs 0-1", 1}, // its first message is the chat's first row
		{"2+2|msgs 2-3", 3},
		{"4+1|msg 4", 5},
		{"5+1|(not summarized yet: zoom it)", 6},
	} {
		i := 0
		for ; i < len(o.Items) && o.Items[i].Text != c.line; i++ {
		}
		if i == len(o.Items) {
			t.Fatalf("no row %q", c.line)
		}
		if got := o.Items[i].Anchor; got != c.want {
			t.Errorf("%s anchors row %d, want %d", c.line, got, c.want)
		}
	}

	// a zoomed child anchors at its own first message
	if err := h.MemoriesCommand("zoom 1 1"); err != nil {
		t.Fatal(err)
	}
	o = h.memoriesOverlay()
	if got := o.Items[1].Anchor; got != 3 || o.Items[2].Anchor != 4 {
		t.Errorf("children anchor %d and %d, want 3 and 4", o.Items[1].Anchor, o.Items[2].Anchor)
	}

	// the window cut down to its tail: the merged lines reach past it and
	// name no row; the tail's leaves still do
	if err := h.MemoriesCommand(""); err != nil { // a fresh browser, the zooms gone
		t.Fatal(err)
	}
	h.SetTranscript(logRows(rows[4:]))
	o = h.memoriesOverlay()
	if got := o.Items[0].Anchor; got != 0 {
		t.Errorf("a line past the window anchors %d", got)
	}
	if got := o.Items[3].Anchor; got != 2 {
		t.Errorf("the tail's leaf anchors %d, want 2", got)
	}
}

// TestMemoriesFoot: the footer reads the view's size, its lines and levels,
// and the unbuilt count.
func TestMemoriesFoot(t *testing.T) {
	t.Parallel()
	h, _ := seedMemories(t)
	o := h.memoriesOverlay()
	// three built lines ("msgs 0-1", "msgs 2-3", "msg 4") at 8+8+5 bytes,
	// one placeholder at its own length, across levels 0 and 1
	if !strings.Contains(o.Foot, "4 lines") || !strings.Contains(o.Foot, "2 levels") || !strings.Contains(o.Foot, "1 unbuilt") {
		t.Errorf("foot = %q", o.Foot)
	}
	if !strings.Contains(o.Foot, "B") {
		t.Errorf("foot names no byte size: %q", o.Foot)
	}
}

// TestMemoriesRange: v reads the messages a line covers, kind-headed, oldest
// first; a line past the log is refused.
func TestMemoriesRange(t *testing.T) {
	t.Parallel()
	h, ui := seedMemories(t)
	if err := h.MemoriesCommand("view 1 0"); err != nil {
		t.Fatal(err)
	}
	o := ui.opened[len(ui.opened)-1]
	if o.ID != "memories range 1 0" || o.Title != "Memories / 0+2" {
		t.Fatalf("range view = %s %q", o.ID, o.Title)
	}
	// each row's head carries its date, the one place a non-coder reaches one
	if !strings.HasPrefix(o.Body, "user 20") || !strings.Contains(o.Body, ": "+strings.Repeat("row ", 200)) {
		t.Errorf("body = %q", clipRunes(o.Body, 120))
	}
	if err := h.MemoriesCommand("view 4 1"); err == nil {
		t.Error("a line past the log opened a view")
	}
}

// TestSeqZeroIsARealMessage: the log's first message is seq 0, not a
// sentinel - the stamping names a live row with it, the jump map and the
// /memories and /messages rows anchor it, zoom opens it, nothing older is
// offered past it.
func TestSeqZeroIsARealMessage(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	// the first turn: the live row lands before its log write
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleUser, Text: "first words"})
	if _, err := h.DB.AppendLog(kindUser, "first words", "", "", ""); err != nil {
		t.Fatal(err)
	}
	h.stampLogSeqs()
	if tr := h.Session().Transcript; tr[0].Seq != 0 {
		t.Fatalf("the first turn's row carries seq %d, want the log's 0", tr[0].Seq)
	}
	// the stamping cursor sits past it, and the jump map answers for it
	h.tMu.RLock()
	from := h.stampFrom
	h.tMu.RUnlock()
	if from != 1 {
		t.Fatalf("the stamping resumes from %d, want 1", from)
	}
	jumps := h.chatJumpRows()
	if got := jumps.at(0, 0); got != 1 {
		t.Fatalf("seq 0 jumps to row %d, want 1", got)
	}

	// the browser's line and the picker's row anchor and name it
	o := h.memoriesOverlay()
	if len(o.Items) != 1 || o.Items[0].Anchor != 1 {
		t.Fatalf("the view lists %+v, want one line anchored at 1", o.Items)
	}
	if err := h.OpenMessages(); err != nil {
		t.Fatal(err)
	}
	list := ui.overlays()[len(ui.overlays())-1]
	if list.Items[0].Anchor != 1 {
		t.Fatalf("the picker's first row = %+v, want the row anchored at 1", list.Items[0])
	}
	if _, ok := h.messagesOlder(); ok {
		t.Error("a page loaded before the log's first message")
	}

	// zoom opens message 0 whole
	if res := zoomCall(t, h, `{"id":0}`); !strings.Contains(res.Content, "first words") {
		t.Fatalf("zoom(0) = %q", res.Content)
	}
}

// TestStampLogSeqs: a real turn stamps the rows it landed in the transcript
// with the log seqs they were written at, so a browser row can jump the chat
// to them.
func TestStampLogSeqs(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	reflectStub(t, h, "pong")
	if err := h.AgentSend("ping"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)
	if err := h.AgentSend("again"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h)

	tr := h.Session().Transcript
	count, err := h.DB.LogCount()
	if err != nil {
		t.Fatal(err)
	}
	if count < 4 { // two prompts, two replies
		t.Fatalf("the turns wrote %d log rows", count)
	}
	logged := map[int64]bool{}
	for seq := range count {
		logged[seq] = true
	}
	stamped := 0
	for _, m := range tr {
		if m.Seq == NoSeq {
			if stampable(m) {
				t.Errorf("row %q stayed unstamped", clipRunes(m.Text, 30))
			}
			continue
		}
		stamped++
		if !logged[m.Seq] {
			t.Errorf("row %q carries seq %d, which the log holds no message for", clipRunes(m.Text, 30), m.Seq)
		}
	}
	if stamped < 4 { // every message named its row, the log's own 0 included
		t.Fatalf("%d rows stamped", stamped)
	}
	// the jump map answers for the newest message
	jumps := h.chatJumpRows()
	if got := jumps.at(count-1, count-1); got == 0 {
		t.Error("the log's newest message names no chat row")
	}
}

// TestStampLogSeqsMismatch: a transcript row whose log write failed pairs
// with nothing - the row draws unstamped and the log's rows pair with the
// rows that did land.
func TestStampLogSeqsMismatch(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	if _, err := h.DB.AppendLog(kindUser, "first", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.AppendLog(kindUser, "later", "", "", ""); err != nil {
		t.Fatal(err)
	}
	// the log holds two rows; the window's first row's write failed
	h.tMu.Lock()
	h.session = &Session{Transcript: []ChatMessage{
		{Role: fantasy.MessageRoleUser, Text: "lost", Seq: NoSeq},
		{Role: fantasy.MessageRoleUser, Text: "later"},
	}}
	h.tMu.Unlock()
	h.stampLogSeqs()
	tr := h.Session().Transcript
	if tr[0].Seq != -1 {
		t.Errorf("the row with no log message carries %d, want the -1 mark", tr[0].Seq)
	}
	if tr[1].Seq != 1 {
		t.Errorf("later stamped %d, want 1", tr[1].Seq)
	}
}
