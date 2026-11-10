package hostapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/store"
)

// TestStampPairsASplitMessage: a message over the chat split logs as sibling
// rows while the transcript holds one; the row names the first chunk and the
// pairing walks past the siblings. Marked gone instead, the cursor stalls on
// the second chunk and the window never pairs again.
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
	// the rebuild folds the marked siblings back into the one row the live
	// window drew: a resumed transcript matches it
	marked := func(seq int64, text string) store.LogEntry {
		return store.LogEntry{Seq: seq, Kind: store.KindUser, Text: text, MetaJSON: `{"split":true}`}
	}
	folded := logRows(nil, []store.LogEntry{marked(0, full[:8000]), marked(1, full[8000:])})
	if len(folded) != 1 || folded[0].Text != full {
		t.Fatalf("the rebuild folds the split's siblings: %+v", folded)
	}
}

// TestStampPairsASplitToolInput: a tool input over the chat split logs as
// sibling rows while the transcript holds the call's one row: the call's row
// names its first chunk, and a later call pairs with its own row - the
// sibling, paired by kind alone, would steal the next call's row.
func TestStampPairsASplitToolInput(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	full := `{"path":"/tmp/plan.md","content":"` + strings.Repeat("x", 9000) + `"}`
	meta := withSplit(`{"id":"call-1","tool":"write"}`)
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Text: toolSummary(nil, "write", full)})
	if _, err := h.DB.AppendLog(store.KindTool, full[:8000], meta, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.AppendLog(store.KindTool, full[8000:], meta, "", ""); err != nil {
		t.Fatal(err)
	}
	h.stampLogSeqs()
	if tr := h.Session().Transcript; tr[0].Seq != 0 {
		t.Fatalf("the call's row names its first chunk: %d", tr[0].Seq)
	}
	h.tMu.RLock()
	from := h.stampFrom
	h.tMu.RUnlock()
	if from != 2 {
		t.Fatalf("the cursor walked past the sibling: %d", from)
	}
	// a later call pairs with its own row: the sibling rides the first chunk
	h.AppendChat(ChatMessage{Role: fantasy.MessageRoleAssistant, Kind: RowTool, Text: toolSummary(nil, "read", `{"path":"/tmp/x"}`)})
	read, err := h.DB.AppendLog(store.KindTool, `{"path":"/tmp/x"}`, `{"id":"call-2","tool":"read"}`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	h.stampLogSeqs()
	if tr := h.Session().Transcript; tr[1].Seq != read {
		t.Fatalf("the next call's row names its own chunk, not the sibling: %d", tr[1].Seq)
	}
	// the rebuild folds a unit's chunks and keeps side-by-side calls apart
	unit := func(seq int64, id, text string) store.LogEntry {
		return store.LogEntry{
			Seq: seq, Kind: store.KindTool, Text: text,
			MetaJSON: `{"id":"` + id + `","split":true,"tool":"write"}`,
		}
	}
	folded := logRows(nil, []store.LogEntry{unit(0, "a", "A1"), unit(1, "a", "A2"), unit(2, "b", "B1"), unit(3, "b", "B2")})
	if len(folded) != 2 || folded[0].Text != "A1A2" || folded[1].Text != "B1B2" {
		t.Fatalf("the rebuild folds each call's chunks, side by side apart: %+v", folded)
	}
}

// TestTheSettleKeepsAFoldedReplyWhole: the settle writes its readout onto a
// split reply's last sibling, over the fold mark its meta holds - a rebuild
// folds the reply into one row that still wears the readout, where a
// wholesale swap would drop the mark and draw the reply as two.
func TestTheSettleKeepsAFoldedReplyWhole(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostDB(t)
	full := strings.Repeat("y", 8100)
	if _, err := h.DB.AppendLog(store.KindUser, "ask", "", "", ""); err != nil {
		t.Fatal(err)
	}
	first, err := h.DB.AppendLog(store.KindAgent, full[:8000], withSplit(""), "", "")
	if err != nil {
		t.Fatal(err)
	}
	last, err := h.DB.AppendLog(store.KindAgent, full[8000:], withSplit(""), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.DB.SetLogMeta(last, `{"turn_ms":7000,"out":50}`); err != nil {
		t.Fatal(err)
	}
	rows, err := h.DB.LogFrom(first, 2)
	if err != nil {
		t.Fatal(err)
	}
	folded := logRows(nil, rows)
	if len(folded) != 1 || folded[0].Text != full {
		t.Fatalf("the settled reply still folds into one row: %+v", folded)
	}
	if folded[0].Stat == nil || folded[0].Stat.ElapsedMS != 7000 || folded[0].Stat.Out != 50 {
		t.Fatalf("the folded reply lost its readout: %+v", folded[0].Stat)
	}
}

// TestLogRowsBrowseKeepsUnpairedEchoes: the transcript folds an attachment's
// echo and a shell command's full output into the row they belong to; the
// browser, where the log's whole truth lives, draws what nothing consumed.
// The logged block opens with the newline its writer joined it with - the
// shape that drew blank rows before firstLine skipped it.
func TestLogRowsBrowseKeepsUnpairedEchoes(t *testing.T) {
	t.Parallel()
	rows := []store.LogEntry{
		{Seq: 0, Kind: store.KindEcho, Text: "\n<file path=\"task.txt\">\nwords\n</file>", MetaJSON: `{"tool":"attach","ok":true}`},
		{Seq: 1, Kind: store.KindUser, Text: "read @task.txt"},
	}
	tr := logRows(nil, rows)
	if len(tr) != 1 {
		t.Fatalf("the transcript folds the attach echo: %+v", tr)
	}
	browse := logRowsFor(nil, rows, true)
	if len(browse) != 2 || browse[0].Kind != RowNotice || browse[0].Text != `<file path="task.txt">` || browse[0].Detail != rows[0].Text || browse[1].Seq != 1 {
		t.Fatalf("the browser draws the unpaired echo in place: %+v", browse)
	}
}

// TestMessageHead: a message reads by its first content line — a fence and a
// bare <zemi-note> wrapper are passed over; a file echo's wrapper is not, its
// path is the honest head.
func TestMessageHead(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, text, want string }{
		{"prose", "done, here is the reply.", "done, here is the reply."},
		{"a reply opening with a block", "```go\nx := 1\n```", "x := 1"},
		{"a system note", "<zemi-note>\n2 todos are open from earlier\n</zemi-note>", "2 todos are open from earlier"},
		{"a file echo keeps its wrapper", "<file path=\"w.txt\">\nwords\n</file>", "<file path=\"w.txt\">"},
		{"a loop echo names its run", "\n\n<attached-files>\n<loop>\ncheck the deploy", "check the deploy"},
		{"a command attachment gives way", "<attached-files>\n<command>\nStudy this repository", "Study this repository"},
	}
	for _, c := range cases {
		if got := messageHead(c.text); got != c.want {
			t.Errorf("%s: head %q, want %q", c.name, got, c.want)
		}
	}
}

// TestEveryLogKindDrawsARow: the store's kinds and logRows' switch are one
// list. A kind the store grows that the switch does not map fails here rather
// than as a message that silently never draws.
func TestEveryLogKindDrawsARow(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("..", "store", "log.go"))
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`Kind\w+\s+LogKind = "(\w+)"`).FindAllStringSubmatch(string(src), -1)
	if len(found) < 2 {
		t.Fatalf("store/log.go names %d kinds: the enum moved", len(found))
	}
	for _, m := range found {
		kind := store.LogKind(m[1])
		row := store.LogEntry{Kind: kind, Text: "a row", MetaJSON: `{"tool":"read","run":"r1","detail":"d","name":"n"}`}
		want := 1
		if kind == store.KindEcho {
			want = 0 // a result rides the call's row, never one of its own
		}
		if got := logRows(func(s string) string { return s }, []store.LogEntry{row}); len(got) != want {
			t.Errorf("kind %q draws %d rows, want %d", kind, len(got), want)
		}
	}
}
