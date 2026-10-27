package store

import (
	"errors"
	"slices"
	"testing"
)

// TestLogSeqDenseAfterCut: seq is the message index, max+1 inside the append
// transaction, so a rewind's tail cut leaves it dense.
func TestLogSeqDenseAfterCut(t *testing.T) {
	db := openTest(t)
	for range 3 {
		if _, err := db.AppendLog("user", "hi", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteLogFrom(2); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := db.AppendLog("user", "back", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.LogTail(10)
	if err != nil {
		t.Fatal(err)
	}
	seqs := make([]int64, len(rows))
	for i, r := range rows {
		seqs[i] = r.Seq
	}
	if !slices.Equal(seqs, []int64{0, 1, 2, 3}) {
		t.Fatalf("seqs after the cut and two appends: %v", seqs)
	}
	if n, err := db.LogCount(); err != nil || n != 4 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

// TestLogAppendReads: the row, its view line and its template node commit
// together, and the windowed reads order oldest first.
func TestLogAppendReads(t *testing.T) {
	db := openTest(t)
	if _, err := db.AppendLog("tool", `{"path":"a.go"}`, `{"tool":"read"}`, "", "tool read a.go"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AppendLog("user", "hello", "", "", ""); err != nil {
		t.Fatal(err)
	}
	// the template node landed in the append transaction
	if n, err := db.NodeAt(0, 0); err != nil || n.Text != "tool read a.go" || n.Size != len(n.Text) {
		t.Fatalf("node(0,0) = %+v, %v", n, err)
	}
	if _, err := db.NodeAt(0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("node(0,1) = %v, want ErrNotFound", err)
	}
	tail, err := db.LogTail(1)
	if err != nil || len(tail) != 1 || tail[0].Seq != 1 || tail[0].Kind != "user" {
		t.Fatalf("tail = %+v, %v", tail, err)
	}
	from, err := db.LogFrom(1, 10)
	if err != nil || len(from) != 1 || from[0].Seq != 1 {
		t.Fatalf("from(1) = %+v, %v", from, err)
	}
	if _, err := db.LogAt(5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LogAt(5) = %v, want ErrNotFound", err)
	}
}

// TestViewMerges: a merge replaces the two children with the parent at the
// first child's ord, skips pairs another window already merged, and leaves
// the coverage exact.
func TestViewMerges(t *testing.T) {
	db := openTest(t)
	for i := range 4 {
		if err := db.AppendViewLine(0, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	// both pairs merge, then a replay of the first runs nowhere: its
	// children are gone
	if n, err := db.MergeViewLines([]ViewMerge{{L: 1, I: 0}, {L: 1, I: 1}}); err != nil || n != 2 {
		t.Fatalf("merged %d, %v, want 2", n, err)
	}
	if n, err := db.MergeViewLines([]ViewMerge{{L: 1, I: 0}}); err != nil || n != 0 {
		t.Fatalf("replayed merge ran %d times, want 0", n)
	}
	lines, err := db.LoadView()
	if err != nil {
		t.Fatal(err)
	}
	type pair struct{ l, i int64 }
	var got []pair
	for _, ln := range lines {
		got = append(got, pair{ln.L, ln.I})
	}
	if !slices.Equal(got, []pair{{1, 0}, {1, 1}}) {
		t.Fatalf("view = %v, want the two level-1 parents", got)
	}
	if n, err := db.MergeViewLines([]ViewMerge{{L: 2, I: 0}}); err != nil || n != 1 {
		t.Fatalf("merged %d, %v, want 1", n, err)
	}
	if lines, _ = db.LoadView(); len(lines) != 1 || lines[0].L != 2 || lines[0].I != 0 {
		t.Fatalf("view = %+v, want the root alone", lines)
	}
}

// TestDeleteLogFrom: the cut takes the log rows from seq on, every node
// whose range intersects them, and the view lines that start there.
func TestDeleteLogFrom(t *testing.T) {
	db := openTest(t)
	for i := range 4 {
		if _, err := db.AppendLog("user", "hi", "", "", ""); err != nil {
			t.Fatal(err)
		}
		if err := db.AppendViewLine(0, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	// a leaf and an ancestor holding the cut
	if _, err := db.InsertNode(0, 3, "leaf"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertNode(2, 0, "root"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteLogFrom(2); err != nil {
		t.Fatal(err)
	}
	if n, err := db.LogCount(); err != nil || n != 2 {
		t.Fatalf("log rows = %d, %v, want 2", n, err)
	}
	if _, err := db.NodeAt(0, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("leaf survived the cut: %v", err)
	}
	if _, err := db.NodeAt(2, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ancestor survived the cut: %v", err)
	}
	lines, err := db.LoadView()
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[1].I != 1 {
		t.Fatalf("view = %+v, want the lines before the cut", lines)
	}
}

// TestMainAnchorRow: the migration plants the billing and todos anchor.
func TestMainAnchorRow(t *testing.T) {
	db := openTest(t)
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, MainSessionID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("anchor rows = %d, %v, want the migration's one row", n, err)
	}
	if s, err := db.GetSession(MainSessionID); err != nil || s.ID != "main" {
		t.Fatalf("anchor row = %+v, %v", s, err)
	}
}
