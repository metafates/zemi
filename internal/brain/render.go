package brain

import (
	"fmt"
	"strings"
	"time"
)

// Caps of the injected # Brain section (§3's table). Backstop is
// hostapi's contextBudget; stage 2 wires the render where that lives.
const (
	CatalogCap = 4 << 10
	WarnCap    = 1 << 10
	Backstop   = 32 << 10
)

// MemHeader is the two contract lines of today's digest header
// (learn/digest.go:18) that stage 2 injects under ## Memory; the digest's
// own "# Memory" H1 is not duplicated (D3).
const MemHeader = "Lines earlier sessions kept, as [id who age]. Follow a preference the user stated there unless this conversation or the project instructions say otherwise. Any other line held when it was written: check the file, command or flag it names before relying on it."

// scopeTitles is the digest's order and wording (learn/digest.go's
// scopeTitles), private partition first.
var scopeTitles = []struct {
	kind  Kind
	title string
}{{Private, "## this project"}, {User, "## every project"}}

// Age is the digest's " 12d" from a line's last write, "" under two days.
func Age(date, at time.Time) string {
	days := int(at.Sub(date).Hours() / 24)
	if days < 2 {
		return ""
	}
	return fmt.Sprintf(" %dd", days)
}

// RenderLine is one memory's injected form, byte-equal to today's digest
// line (learn/digest.go:51).
func RenderLine(l Line, at time.Time) string {
	return fmt.Sprintf("- [%s %s%s] %s", l.ID, l.Origin, Age(l.Date, at), l.Text)
}

// Catalog renders one partition's derived catalog (§3): one line per file,
// sorted by name, each field stripped and capped at render. A file that
// cannot serve - no frontmatter, unreadable - renders a one-line
// self-repair note in its place. Past CatalogCap the tail is cut
// alphabetically last and the marker names the partition.
func Catalog(l Loaded) string {
	lines := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		lines = append(lines, catalogLine(e))
	}
	var marker string
	if size(lines) > CatalogCap {
		cut := 0
		for len(lines) > 0 {
			cut++
			marker = fmt.Sprintf("- ... %d more in %s", cut, l.Partition.Dir)
			lines = lines[:len(lines)-1]
			// the marker rides inside the cap, so the cut never overshoots
			if size(lines)+len(marker)+1 <= CatalogCap {
				break
			}
		}
	} else if l.Over {
		// the scan ceiling dropped files the entries never show; the
		// marker names the partition (D6) and rides inside the cap
		marker = "- ... more in " + l.Partition.Dir
		for len(lines) > 0 && size(lines)+len(marker)+1 > CatalogCap {
			lines = lines[:len(lines)-1]
		}
	}
	if marker != "" {
		lines = append(lines, marker)
	}
	return strings.Join(lines, "\n")
}

func catalogLine(e Entry) string {
	// the rendered path strips like the fields, so a filename can neither
	// break the one-line format nor carry multi-line text (D3)
	name := Normalize(e.Name)
	desc, read := Normalize(e.FM.Description), Normalize(e.FM.ReadIf)
	if desc == "" || read == "" {
		note := "needs description and read_if frontmatter; add them so this file can trigger"
		if e.Err != nil {
			note = errLine(e.Err) + "; add description and read_if frontmatter"
		}
		return "- " + name + " - (" + note + ")"
	}
	return "- " + name + " - " + desc + ". (Read If: " + read + ".)"
}

// Memory renders one partition's memories.md (D4): the digest's lines,
// cut at ScopeBudget when a hand-edited or corrupted file overflows it,
// the marker naming memories.md. Unreadable is a marker, never an error.
func Memory(l Loaded, at time.Time) string {
	if l.MemErr != nil {
		return fmt.Sprintf("- ... %s is unreadable (%s)", MemFile, strip(l.MemErr.Error()))
	}
	lines, bad := ParseMemories(l.Mem)
	if len(lines) == 0 && len(bad) == 0 {
		return ""
	}
	used, kept := 0, 0
	for _, ln := range lines {
		if used += LineSize(ln.Text); used > ScopeBudget {
			break
		}
		kept++
	}
	out := make([]string, 0, kept+1)
	for _, ln := range lines[:kept] {
		out = append(out, RenderLine(ln, at))
	}
	if cut := len(lines) + len(bad) - kept; cut > 0 {
		out = append(out, fmt.Sprintf("- ... %d more in %s", cut, l.Partition.memPath()))
	}
	return strings.Join(out, "\n")
}

// Warnings renders the findings as <brain-warning> lines (§3): over
// WarnCap the last finding is dropped, then the remaining line is cut to
// fit. Findings are zemi-generated text; the strip is defense in depth.
func Warnings(findings []Finding) string {
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, "<brain-warning>"+strip(f.Text)+"</brain-warning>")
	}
	for len(lines) > 1 && size(lines) > WarnCap {
		lines = lines[:len(lines)-1]
	}
	const wrap = len("<brain-warning></brain-warning>")
	if len(lines) == 1 && wrap+size(lines) > WarnCap {
		text := []rune(lines[0][len("<brain-warning>") : len(lines[0])-len("</brain-warning>")])
		for len(text) > 0 && wrap+len(string(text)) > WarnCap {
			text = text[:len(text)-1]
		}
		lines[0] = "<brain-warning>" + string(text) + "</brain-warning>"
	}
	return strings.Join(lines, "\n")
}

// Render assembles the # Brain section (§4): the loop text, the findings,
// the partitions' catalogs, user first, and the memory lines, private
// scope first. Every piece is cut per §3's table and the whole section to
// Backstop with the catalogs cut to zero first, so the cut order
// terminates: the loop, the warnings and the memory lines always fit.
func Render(loop string, parts []Loaded, findings []Finding, at time.Time) string {
	full := render(loop, parts, findings, at, 0)
	if len(full) <= Backstop {
		return full
	}
	if one := render(loop, parts, findings, at, 1); len(one) <= Backstop {
		return one
	}
	return render(loop, parts, findings, at, 2)
}

func render(loop string, parts []Loaded, findings []Finding, at time.Time, drop int) string {
	var b strings.Builder
	b.WriteString("# Brain\n")
	if loop != "" {
		b.WriteString("\n" + strings.TrimRight(loop, "\n") + "\n")
	}
	if w := Warnings(findings); w != "" {
		b.WriteString("\n" + w + "\n")
	}
	catalog := catalogSections(parts, drop)
	if catalog != "" {
		b.WriteString("\n## Catalog\n" + catalog)
	}
	memory := memorySections(parts, at)
	if memory != "" {
		b.WriteString("\n## Memory\n" + MemHeader + "\n" + memory)
	}
	if out := strings.TrimRight(b.String(), "\n"); out != "# Brain" {
		return out
	}
	return ""
}

// catalogSections is the ### user and ### private blocks, in that order;
// drop zeroes that many catalogs, backstop first. A partition with no
// files renders no block at all.
func catalogSections(parts []Loaded, drop int) string {
	var b strings.Builder
	dropped := 0
	for _, k := range []Kind{User, Private} {
		l, ok := loadedOf(parts, k)
		if !ok {
			continue
		}
		c := Catalog(l)
		if c == "" && dropped >= drop {
			continue
		}
		b.WriteString("\n### " + k.String() + "\n")
		if dropped < drop {
			b.WriteString("- ... the catalog of " + l.Partition.Dir + " is cut here\n")
			dropped++
			continue
		}
		b.WriteString(c + "\n")
	}
	return b.String()
}

// memorySections is the scope blocks, private first, the digest's own
// order.
func memorySections(parts []Loaded, at time.Time) string {
	var b strings.Builder
	for _, sc := range scopeTitles {
		l, ok := loadedOf(parts, sc.kind)
		if !ok {
			continue
		}
		if lines := Memory(l, at); lines != "" {
			b.WriteString("\n" + sc.title + "\n" + lines + "\n")
		}
	}
	return b.String()
}

func loadedOf(parts []Loaded, k Kind) (Loaded, bool) {
	for _, l := range parts {
		if l.Partition.Kind == k {
			return l, true
		}
	}
	return Loaded{}, false
}

// size is the joined lines' byte length, newlines between included.
func size(lines []string) int {
	if len(lines) == 0 {
		return 0
	}
	n := len(lines) - 1
	for _, l := range lines {
		n += len(l)
	}
	return n
}
