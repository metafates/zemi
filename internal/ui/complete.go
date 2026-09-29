package ui

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/tools"
)

// fileCompleter is the composer popup after a trigger that starts a word:
// "@" lists project files, scanned off the UI goroutine and ranked here,
// and any trigger lists what its Lua sources offer (extra, first).
type fileCompleter struct {
	open     bool
	trigger  rune
	start    int      // rune index of the trigger char in the composer
	token    string   // query between the trigger and the cursor
	files    []string // paths relative to the session cwd
	bases    []string // each file's last element
	matches  []hostapi.Hit
	extra    []listItem // the Lua sources' items, ahead of matches
	selected int
	view     viewport
	size     boxSize // typing never narrows or shortens the popup

	scanning bool
	walkGen  int  // bumps so stale scan results are dropped
	partial  bool // files match that the list leaves out, hence the note
}

// scanMaxFiles caps the scan so a walk that never ends cannot eat memory.
// A repo that passes it loses the paths listed last, hence the note.
const scanMaxFiles = 50000

// listMaxFiles caps the matches the popup lists: every frame builds the
// whole list, and nobody scrolls past a few hundred paths to find one.
const listMaxFiles = 200

func (a *App) completionEnabled() bool { return a.host.Settings.Bool("ui.completion.files") }

// completionMax is the rows the popup shows, clamped to what a popup can show.
func (a *App) completionMax() int {
	return min(max(a.host.Settings.Int("ui.completion.max_items"), 1), 50)
}

// updateCompletion recomputes both popups after a composer edit: the
// palette, and the completion whose token is the text between a trigger
// that starts a word and the cursor; anything else closes it. Lua sources
// run on the worker, never on the key path. The returned cmd launches a
// file scan when an "@" popup just opened.
func (a *App) updateCompletion() tea.Cmd {
	a.updatePalette()
	r := a.composer.runes()
	cur := a.composer.cursor
	i := cur
	for i > 0 && r[i-1] != '\n' && r[i-1] != ' ' && r[i-1] != '\t' {
		i--
	}
	files := i < cur && r[i] == '@' && a.completionEnabled()
	lua := i < cur && a.host.HasCompletions(string(r[i]), "")
	if !files && !lua {
		a.completion.close()
		return nil
	}
	f := &a.completion
	opening := !f.open || f.trigger != r[i]
	if opening {
		f.close()
	}
	f.start, f.trigger, f.open, f.selected = i, r[i], true, 0
	f.token = string(r[i+1 : cur])
	if lua {
		// until the sources answer, the rows they gave for a shorter word
		// narrow, so a fast tab never takes a row the word left
		f.extra = matchRows(f.extra, f.token, true)
		trigger, token, line := string(r[i]), f.token, string(r[:cur])
		a.background(func() {
			items := a.host.Complete(context.Background(), trigger, "", token, line)
			a.do(func() {
				if !f.open || a.composer.cursor > len(a.composer.runes()) || string(a.composer.runes()[:a.composer.cursor]) != line {
					return // the draft moved on
				}
				f.extra = matchRows(candRows(items), token, false) // shown as the sources matched them
				f.selected = max(0, min(f.selected, f.count()-1))
			})
		})
	}
	if !files {
		return nil
	}
	f.rematch()
	// Rescan on every open so files created since show up; the old list
	// stays until the new one lands.
	if opening {
		a.completion.scanning = true
		a.completion.walkGen++
		return a.scanFilesCmd(a.host.Session().CWD, a.completion.walkGen)
	}
	return nil
}

// onFilesScanned stores scan results unless a newer scan superseded them.
func (a *App) onFilesScanned(msg filesScannedMsg) {
	if msg.gen != a.completion.walkGen {
		return
	}
	a.completion.scanning = false
	a.completion.setFiles(msg.files)
	if a.completion.open {
		a.completion.rematch()
	}
}

func (f *fileCompleter) setFiles(files []string) {
	f.files, f.bases = files, make([]string, len(files))
	for i, p := range files {
		f.bases[i] = p[strings.LastIndexByte(p, '/')+1:]
	}
}

func (f *fileCompleter) close() {
	f.open = false
	f.token = ""
	f.selected, f.view, f.size = 0, viewport{}, boxSize{}
	f.matches, f.extra = f.matches[:0], nil
}

func (f *fileCompleter) count() int { return len(f.extra) + len(f.matches) }

// shown reports whether the popup is drawn and takes its keys: a Lua
// trigger with nothing to offer stays out of the way of prose.
func (f *fileCompleter) shown() bool { return f.open && (f.trigger == '@' || f.count() > 0) }

// rematch ranks the files the token matches, a file name it begins first.
// A name it matches scores by the name alone, which a path's other elements
// would only blur; a file matched across directories scores by its path.
func (f *fileCompleter) rematch() {
	if f.token == "" {
		f.matches = hostapi.Match("", f.files[:min(len(f.files), listMaxFiles)], nil)
		f.partial = len(f.files) > listMaxFiles || len(f.files) >= scanMaxFiles
		f.selected = max(0, min(f.selected, f.count()-1))
		return
	}
	// chunks of the tree match at once: scoring the thousands of hits a short
	// token has in a big tree takes longer than a keystroke may on one core
	const chunk = 8192
	best := make([][]hostapi.Hit, (len(f.files)+chunk-1)/chunk)
	var hits atomic.Int64
	var wg sync.WaitGroup
	for c := range best {
		wg.Go(func() {
			var n int
			best[c], n = f.matchFiles(c*chunk, min(c*chunk+chunk, len(f.files)))
			hits.Add(int64(n))
		})
	}
	wg.Wait()
	f.partial = hits.Load() > listMaxFiles || len(f.files) >= scanMaxFiles
	f.matches = hostapi.Rank(slices.Concat(best...), f.files, listMaxFiles)
	f.selected = max(0, min(f.selected, f.count()-1))
}

// matchFiles matches the token against files[lo:hi]: the best listMaxFiles
// hits, indexed into files, and how many there were.
func (f *fileCompleter) matchFiles(lo, hi int) ([]hostapi.Hit, int) {
	files, bases := f.files[lo:hi], f.bases[lo:hi]
	names := hostapi.Match(f.token, bases, nil)
	rest := slices.Clone(files) // blanked where the name matched: "" matches nothing
	for _, h := range names {
		rest[h.Index] = ""
		for k := range h.Pos {
			h.Pos[k] += len(files[h.Index]) - len(bases[h.Index])
		}
	}
	paths := hostapi.Match(f.token, rest, nil)
	for i := range paths {
		paths[i].Prefix = false // only a name's prefix leads
	}
	hits := slices.Concat(names, paths)
	for i := range hits {
		hits[i].Index += lo
	}
	return hostapi.Rank(hits, f.files, listMaxFiles), len(hits)
}

// candRows are completion candidates as list rows.
func candRows(cands []hostapi.CompletionItem) []listItem {
	rows := make([]listItem, 0, len(cands))
	for _, c := range cands {
		rows = append(rows, listItem{label: c.Text, desc: c.Desc})
	}
	return rows
}

// matchRows marks what query matches in each row's label, in row order;
// narrow also drops the rows it does not match.
func matchRows(rows []listItem, query string, narrow bool) []listItem {
	labels := make([]string, len(rows))
	for i, r := range rows {
		labels[i] = r.label
	}
	hits := hostapi.Match(query, labels, nil)
	out := make([]listItem, 0, len(rows))
	for i, r := range rows {
		r.match = nil
		if len(hits) > 0 && hits[0].Index == i {
			r.match, hits = hits[0].Pos, hits[1:]
		} else if narrow {
			continue
		}
		out = append(out, r)
	}
	return out
}

// accept replaces the token after the trigger with the selected match (plus
// a trailing space) and closes the popup. The trigger stays: "@path" is what
// attaches the file.
func (a *App) acceptCompletion() {
	f := &a.completion
	var text string
	switch n := len(f.extra); {
	case !f.open || f.selected >= f.count():
		return
	case f.selected < n:
		text = f.extra[f.selected].label
	default:
		text = f.files[f.matches[f.selected-n].Index]
	}
	a.composer.replace(f.start+1, text+" ")
	f.close()
}

// completionRows renders the popup's list, at most room rows of at most
// maxW cells, padded to minW.
func (a *App) completionRows(maxW, room, minW int) [][]hostapi.Segment {
	f := &a.completion
	note := func(text string) []hostapi.Segment {
		return []hostapi.Segment{{Text: text, HL: hostapi.ChromeHint}}
	}
	if f.count() == 0 && f.scanning && len(f.files) == 0 {
		return [][]hostapi.Segment{note("scanning files…")}
	}
	partial := note("more files than the list holds - type more of the path")
	switch {
	case f.count() == 0 && f.partial:
		return [][]hostapi.Segment{partial}
	case f.count() == 0:
		return [][]hostapi.Segment{note("No matches.")}
	case f.partial:
		room--
	}
	items := append([]listItem(nil), f.extra...)
	for _, m := range f.matches {
		items = append(items, listItem{label: f.files[m.Index], match: m.Pos})
	}
	rows := listRowsW(items, items, f.selected, &f.view, min(a.completionMax(), room), minW, maxW)
	if f.partial {
		rows = append(rows, partial)
	}
	return rows
}

// scanFilesCmd lists the project's files off the UI goroutine.
func (a *App) scanFilesCmd(dir string, gen int) tea.Cmd {
	return func() tea.Msg {
		files, _, _ := tools.ListFiles(context.Background(), dir, scanMaxFiles)
		return filesScannedMsg{gen: gen, files: files}
	}
}

type filesScannedMsg struct {
	gen   int
	files []string
}
