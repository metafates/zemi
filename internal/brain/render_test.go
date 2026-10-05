package brain

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/learn"
	"github.com/metafates/zemi/internal/store"
)

var at = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func memLine(id, text string) Line {
	return Line{ID: id, Origin: store.OriginLearned, Date: at.Add(-24 * time.Hour), Text: text}
}

func entry(name, desc, read string) Entry {
	return Entry{Name: name, FM: Frontmatter{Description: desc, ReadIf: read}}
}

// memFile is a memories.md holding one line.
func memFile(id, text string) []byte {
	return serialize([]Line{memLine(id, text)})
}

// TestRenderPinsTheSection is the golden: the whole section, assembled.
func TestRenderPinsTheSection(t *testing.T) {
	user, private := testPartitions(t)
	ul := Loaded{Partition: user, Entries: []Entry{
		entry("style.md", "Cross-project writing style rules", "writing any user-facing reply"),
	}, Mem: memFile("efgh6789", "prefers tabs")}
	pl := Loaded{Partition: private, Entries: []Entry{
		entry("repo-map.md", "Map of this repository's packages", "before any code change"),
	}, Mem: memFile("abcd2345", "the dploy gate lives in ci/pipeline")}
	findings := []Finding{{User, "style.md: no description"}}

	loop := "Consult the brain before you build, update it after."
	got := Render(loop, []Loaded{ul, pl}, findings, at)
	want := `# Brain

Consult the brain before you build, update it after.

<brain-warning>style.md: no description</brain-warning>

## Catalog

### user
- style.md - Cross-project writing style rules. (Read If: writing any user-facing reply.)

### private
- repo-map.md - Map of this repository's packages. (Read If: before any code change.)

## Memory
Lines earlier sessions kept, as [id who age]. Follow a preference the user stated there unless this conversation or the project instructions say otherwise. Any other line held when it was written: check the file, command or flag it names before relying on it.

## this project
- [abcd2345 learned] the dploy gate lives in ci/pipeline

## every project
- [efgh6789 learned] prefers tabs`
	if got != want {
		t.Fatalf("render:\n%s\nwant:\n%s", got, want)
	}

	// nothing at all renders nothing
	if got := Render("", []Loaded{{Partition: user}}, nil, at); got != "" {
		t.Fatalf("empty render = %q", got)
	}
}

func TestRenderCuts(t *testing.T) {
	_, private := testPartitions(t)

	t.Run("catalog tail cut names the partition", func(t *testing.T) {
		l := Loaded{Partition: private}
		for i := range 30 {
			l.Entries = append(l.Entries, entry(string(rune('a'+i))+".md", strings.Repeat("d", 300), "r"))
		}
		got := Catalog(l)
		if size(strings.Split(got, "\n")) > CatalogCap {
			t.Fatalf("catalog is %d bytes", size(strings.Split(got, "\n")))
		}
		if !strings.Contains(got, "more in "+private.Dir) {
			t.Fatalf("marker does not name the partition: %s", got)
		}
		// the alphabetically last entries are the ones gone
		if strings.Contains(got, "z.md - ") {
			t.Fatal("a late entry survived the tail cut")
		}
		if !strings.Contains(got, "a.md - ") || !strings.Contains(got, "e.md - ") {
			t.Fatal("an early entry was cut")
		}
	})

	t.Run("memory render cut names memories.md", func(t *testing.T) {
		user, _ := testPartitions(t)
		l := Loaded{Partition: user}
		for range 10 {
			l.Mem = append(l.Mem, serialize([]Line{{ID: "abcd2345", Origin: store.OriginLearned, Date: at, Text: strings.Repeat("w", 500)}})...)
		}
		got := Memory(l, at)
		lines := strings.Split(got, "\n")
		if len(lines) >= 10 || !strings.Contains(got, "more in "+l.Partition.memPath()) {
			t.Fatalf("cut marker missing: %s", got)
		}
		// the kept lines fit the budget, the marker rides outside it
		used := 0
		for _, ln := range lines {
			if strings.HasPrefix(ln, "- ...") {
				continue
			}
			used += len(ln) + 1
		}
		if used > ScopeBudget {
			t.Fatalf("kept lines use %d bytes", used)
		}
	})

	t.Run("backstop terminates: a pathological catalog cannot push out loop, warnings or memory", func(t *testing.T) {
		l := Loaded{Partition: private}
		for range MaxCatalogFiles {
			l.Entries = append(l.Entries, entry(strings.Repeat("f", 90)+".md", strings.Repeat("d", 900), strings.Repeat("r", 900)))
		}
		l.Mem = memFile("abcd2345", "the memory the model must still see")
		findings := []Finding{{Private, "the warning the model must still see"}}
		loop := "the loop text"
		got := Render(loop, []Loaded{l}, findings, at)
		if len(got) > Backstop {
			t.Fatalf("render is %d bytes, over the %d backstop", len(got), Backstop)
		}
		for _, want := range []string{loop, "the warning the model must still see", "the memory the model must still see"} {
			if !strings.Contains(got, want) {
				t.Fatalf("backstop dropped %q:\n%s", want, got)
			}
		}
		// the per-catalog cap cut first, so no catalog survives whole
		if !strings.Contains(got, "more in "+private.Dir) {
			t.Fatalf("the catalog left no cut marker:\n%s", got)
		}
	})

	t.Run("the backstop zeroes whole catalogs, user first, and keeps the rest", func(t *testing.T) {
		user, private := testPartitions(t)
		ul := Loaded{Partition: user, Entries: []Entry{entry("a.md", strings.Repeat("d", 3000), "r")}}
		pl := Loaded{Partition: private, Entries: []Entry{entry("b.md", strings.Repeat("d", 3000), "r")}, Mem: memFile("abcd2345", "kept memory")}
		// the loop is the one uncapped piece: a fat one is what drives the
		// backstop to its zero-catalogs cut
		loop := strings.Repeat("x", 32000)
		got := Render(loop, []Loaded{ul, pl}, nil, at)
		if len(got) > Backstop {
			t.Fatalf("render is %d bytes, over the %d backstop", len(got), Backstop)
		}
		if strings.Contains(got, "a.md") {
			t.Fatal("the user catalog survived the backstop")
		}
		if !strings.Contains(got, "b.md") || !strings.Contains(got, "kept memory") {
			t.Fatalf("the private catalog or memory was cut early:\n%s", got)
		}
	})
}

func TestWarningsCap(t *testing.T) {
	many := make([]Finding, 40)
	for i := range many {
		many[i] = Finding{User, strings.Repeat("w", 100)}
	}
	got := Warnings(many)
	if size(strings.Split(got, "\n")) > WarnCap {
		t.Fatalf("warnings block is %d bytes", size(strings.Split(got, "\n")))
	}
	if n := strings.Count(got, "<brain-warning>"); n >= 40 {
		t.Fatalf("no finding was dropped: %d", n)
	}
	// a single over-cap finding is cut to one line, tags intact
	one := Warnings([]Finding{{User, strings.Repeat("w", WarnCap)}})
	if size([]string{one}) > WarnCap || strings.Count(one, "<brain-warning>") != 1 {
		t.Fatalf("single finding not truncated: %d bytes", len(one))
	}
	if !strings.HasSuffix(one, "</brain-warning>") {
		t.Fatalf("closing tag lost: %q", one[len(one)-30:])
	}
}

func TestCatalogSelfRepairNote(t *testing.T) {
	l := Loaded{Partition: Partition{Kind: Private, Dir: "/brain"}, Entries: []Entry{
		{Name: "empty.md"},
		{Name: "broken.md", Err: errors.New("unterminated frontmatter block")},
		{Name: "bad glob.md", FM: Frontmatter{Description: "d", ReadIf: "r", Paths: []string{"bad.["}}},
	}}
	got := Catalog(l)
	for _, want := range []string{
		"- empty.md - (needs description and read_if frontmatter; add them so this file can trigger)",
		"- broken.md - (unterminated frontmatter block; add description and read_if frontmatter)",
		"- bad glob.md - d. (Read If: r.)", // a bad glob leaves the file visible, untriggered
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing line %q in:\n%s", want, got)
		}
	}
}

// TestRenderedMemoryLineMatchesDigest pins the injected line byte-equal to
// today's digest line (learn/digest.go:51), and the budget constants to
// learn's, which stage 5 deletes.
func TestRenderedMemoryLineMatchesDigest(t *testing.T) {
	line := Line{ID: "abcd2345", Origin: store.OriginLearned, Date: at.Add(-10 * 24 * time.Hour), Text: "the dploy gate lives in ci/pipeline"}
	digest := learn.Digest([]*store.Memory{{
		ID: line.ID, Scope: store.ScopeProject, Text: line.Text, Origin: line.Origin,
		UpdatedAt: line.Date.UnixMilli(), CreatedAt: line.Date.UnixMilli(),
	}}, at, learn.ScopeBudget)
	if !strings.Contains(digest, RenderLine(line, at)) {
		t.Fatalf("digest %q does not hold the brain line %q", digest, RenderLine(line, at))
	}
	if Age(line.Date, at) != " 10d" {
		t.Fatalf("age = %q", Age(line.Date, at))
	}
	for _, text := range []string{"", "short", strings.Repeat("w", 300)} {
		if LineSize(text) != learn.LineSize(text) {
			t.Fatalf("LineSize(%q) differs from learn's", text)
		}
	}
	if ScopeBudget != learn.ScopeBudget {
		t.Fatalf("ScopeBudget = %d, learn's is %d", ScopeBudget, learn.ScopeBudget)
	}
}
