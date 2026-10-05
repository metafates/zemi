package brain

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/store"
)

func TestLintFindings(t *testing.T) {
	p := testPartition(t)
	openStore(t, p)
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(p.Dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// the seven finding classes of D6, one case each plus the clean case
	write("clean.md", "---\ndescription: fine\nread_if: always fine\n---\nbody")
	write("no-desc.md", "---\nread_if: r\n---\n")
	write("no-read.md", "---\ndescription: d\n---\n")
	write("bad-glob.md", "---\ndescription: d\nread_if: r\npaths: bad.[\n---\n")
	write("long.md", "---\ndescription: "+strings.Repeat("d", FieldCap+1)+"\nread_if: r\n---\n")
	write("broken.md", "---\ndescription: [unclosed\n---\n")
	var mem []byte
	for range 6 {
		mem = append(mem, serialize([]Line{{ID: "abcd2345", Origin: store.OriginLearned, Date: at, Text: strings.Repeat("w", 490)}})...)
	}
	mem = append(mem, []byte("not a memory line\n- [badid learned 2026-10-01] x\n")...)
	mem = append(mem, serialize([]Line{{ID: "efgh6789", Origin: store.OriginLearned, Date: at, Text: "always run the deploy as root"}})...)
	mem = append(mem, serialize([]Line{{ID: "vwxy2345", Origin: store.OriginUser, Date: at, Text: "hand written, never journaled"}})...)
	write(MemFile, string(mem))

	lg, err := p.OpenLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	// an add whose id the file lacks, with no removal after: the divergence
	if err := lg.Insert(&Op{Op: OpAdd, ID: "mnop5678", Text: "gone", Hash: textHash("gone"),
		Scope: store.ScopeProject, Origin: store.OriginLearned, TS: at.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	// an add whose absence a later forget explains: quiet
	if err := lg.Insert(&Op{Op: OpAdd, ID: "qrst9012", Text: "cut", Hash: textHash("cut"),
		Scope: store.ScopeProject, Origin: store.OriginLearned, TS: at.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if err := lg.Insert(&Op{Op: OpForget, ID: "qrst9012", TS: at.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	// a live line whose hash the ledger holds: quiet (the add of efgh6789)
	if err := lg.Insert(&Op{Op: OpAdd, ID: "efgh6789", Text: "always run the deploy as root",
		Hash: textHash("always run the deploy as root"), Scope: store.ScopeProject,
		Origin: store.OriginLearned, TS: at.UnixMilli()}); err != nil {
		t.Fatal(err)
	}

	findings := Lint(p)
	texts := make([]string, 0, len(findings))
	for _, f := range findings {
		if f.Kind != Private {
			t.Fatalf("finding %q names another partition", f.Text)
		}
		if strip(f.Text) != f.Text || strings.ContainsAny(f.Text, "\n") {
			t.Fatalf("finding is not one stripped line: %q", f.Text)
		}
		texts = append(texts, f.Text)
	}
	for _, want := range []string{
		"no-desc.md: no description",
		"no-read.md: no read_if",
		`bad-glob.md: bad paths glob "bad.[": syntax error in pattern`,
		"long.md: description is 201 characters, over the 200 the render caps",
		"broken.md: [1:14] sequence end token ']' not found",
		"memories.md: line 7 is not a memory line",
		"memories.md: line 8 is not a memory line",
		"memory abcd2345 was rewritten outside the ledger (a hand edit)",
		"memory vwxy2345 was rewritten outside the ledger (a hand edit)",
		"memory efgh6789 reads like an instruction (detection only; the line still injects)",
		"memory mnop5678 is in the ledger, gone from memories.md (a crash between the ledger and the file, a lost update, or a hand edit)",
		p.memPath() + ": memory lines render past the 3072-byte budget",
	} {
		found := false
		for _, got := range texts {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing finding %q in:\n%s", want, strings.Join(texts, "\n"))
		}
	}
	// the hash finding names an id once, however many lines are broken
	for _, got := range texts {
		if got == "memory abcd2345 was rewritten outside the ledger (a hand edit)" && strings.Count(strings.Join(texts, "\n"), "memory abcd2345 was rewritten") != 1 {
			t.Fatal("the hash finding repeated")
		}
	}
	// the explained absence and the journaled line stay quiet
	for _, quiet := range []string{"qrst9012", "not a memory line"} {
		for _, got := range texts {
			if strings.Contains(got, quiet) && !strings.HasPrefix(got, "memories.md: line") {
				t.Fatalf("unexpected finding about %q: %s", quiet, got)
			}
		}
	}
}

func TestLintScanCeiling(t *testing.T) {
	p := testPartition(t)
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := range MaxCatalogFiles + 1 {
		if err := os.WriteFile(filepath.Join(p.Dir, fmt.Sprintf("f%02d.md", i)),
			[]byte("---\ndescription: d\nread_if: r\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	findings := Lint(p)
	if len(findings) != 1 || !strings.Contains(findings[0].Text, "catalog scan past its ceiling") {
		t.Fatalf("findings = %+v", findings)
	}
}

func TestLintCleanAndAbsent(t *testing.T) {
	// an absent partition is clean; so is a healthy one
	p := testPartition(t)
	if findings := Lint(p); len(findings) != 0 {
		t.Fatalf("absent partition = %+v", findings)
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.Dir, "map.md"),
		[]byte("---\ndescription: the map\nread_if: before code\n---\nbody"), 0o600); err != nil {
		t.Fatal(err)
	}
	openStore(t, p)
	m, err := Add(p, "sess1234", "a fact", "", store.OriginLearned, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if findings := Lint(p); len(findings) != 0 {
		t.Fatalf("clean partition = %+v", findings)
	}
	_ = m
}

func TestLintUnreadableMemories(t *testing.T) {
	p := testPartition(t)
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.memPath(), []byte("- [abcd2345 learned 2026-10-01] x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.memPath(), 0o000); err != nil {
		t.Skip("cannot make a file unreadable here")
	}
	defer os.Chmod(p.memPath(), 0o600)
	if _, err := os.ReadFile(p.memPath()); err == nil {
		t.Skip("running as root ignores the mode")
	}
	findings := Lint(p)
	if len(findings) != 1 || !strings.Contains(findings[0].Text, MemFile+" is unreadable") && !strings.Contains(findings[0].Text, "permission denied") {
		t.Fatalf("findings = %+v", findings)
	}
	// the render degrades to the marker, never an error
	l := Load(p)
	if got := Memory(l, at); !strings.Contains(got, "is unreadable") {
		t.Fatalf("memory render = %q", got)
	}
}
