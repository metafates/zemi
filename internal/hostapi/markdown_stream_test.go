package hostapi

import (
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

// streamDoc is one long reply: headings, prose, a list, a code fence, a
// tilde fence holding pipe rows and a long list, long enough that
// settledPrefix splits it while streaming.
func streamDoc() string {
	var b strings.Builder
	for i := range 30 {
		b.WriteString("## Section " + strconv.Itoa(i) + "\n\n")
		b.WriteString("A prose paragraph with enough words to wrap across the width of the chat pane, mentioning details " + strconv.Itoa(i) + " and more.\n\n")
		if i%3 == 0 {
			b.WriteString("- bullet one with words\n- another bullet here\n- and a third one\n\n")
		}
		if i%3 == 1 {
			b.WriteString("```go\nfunc example" + strconv.Itoa(i) + "() int {\n\treturn " + strconv.Itoa(i) + " * 2 // doubled\n}\n```\n\n")
			b.WriteString("~~~\n| not | a | table |\n|---|---|---|\n\nstill ~~~ fenced, not a table\n\n~~~\n\n")
		}
		if i%3 == 2 {
			b.WriteString("| col a | col b | col c |\n|---|---|---|\n| x" + strconv.Itoa(i) + " | y" + strconv.Itoa(i) + " | z" + strconv.Itoa(i) + " |\n\n")
		}
		if i%7 == 3 {
			for j := range 12 {
				b.WriteString("- item " + strconv.Itoa(i) + "." + strconv.Itoa(j) + " with a few words apiece to stretch the list\n")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// TestMarkdownStreamStitch renders a reply one shot and then the way a
// stream does, growing line by line, and requires the same segments at every
// step: the stitched prefix+tail must be exactly the whole-text render, and
// the cache's scratch must not leak between calls.
func TestMarkdownStreamStitch(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	t.Cleanup(h.Close)
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	full := streamDoc()
	want := h.Markdown(full, 100)

	// a fresh host renders each step without the streaming slot, so the
	// comparison pins the stitched path against a whole-text render
	r2 := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h2 := NewHost(r2, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	t.Cleanup(h2.Close)
	if err := r2.Boot(t.Context(), []lua.BootStep{h2.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}

	lines := strings.SplitAfter(full, "\n")
	for n := range lines {
		at := len(strings.Join(lines[:n], ""))
		if at == 0 {
			continue
		}
		got := h.Markdown(full[:at], 100)
		ref := h2.Markdown(full[:at], 100)
		if !segsLinesEqual(got, ref) {
			t.Fatalf("streamed render differs from a whole render at %d/%d bytes (line %d)", at, len(full), n)
		}
	}
	if !segsLinesEqual(want, h2.Markdown(full, 100)) {
		t.Fatal("the whole-text render differs from the fresh-host render")
	}
}

func segsLinesEqual(a, b [][]Segment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return false
		}
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				return false
			}
		}
	}
	return true
}

// BenchmarkMarkdownStream renders one long reply the way a turn streams it:
// every delta re-renders the live text, as the frame tick does.
func BenchmarkMarkdownStream(b *testing.B) {
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: b.TempDir()})
	b.Cleanup(h.Close)
	if err := r.Boot(b.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		b.Fatal(err)
	}
	words := strings.Fields(streamDoc())
	b.ResetTimer()
	for b.Loop() {
		var sb strings.Builder
		for _, w := range words {
			sb.WriteString(w)
			sb.WriteByte(' ')
			h.Markdown(sb.String(), 100)
		}
	}
}
