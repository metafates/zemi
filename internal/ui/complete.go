package ui

import (
	"cmp"
	"context"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/tools"
)

// fileCompleter is the composer popup after a trigger that starts a word:
// "@" lists project files, scanned off the UI goroutine and fuzzy-ranked
// here, and any trigger lists what its Lua sources offer (extra, first).
type fileCompleter struct {
	open     bool
	trigger  rune
	start    int      // rune index of the trigger char in the composer
	token    string   // query between the trigger and the cursor
	files    []string // paths relative to the session cwd
	matches  []string
	extra    []listItem // the Lua sources' items, ahead of matches
	selected int
	view     viewport

	scanning bool
	walkGen  int  // bumps so stale scan results are dropped
	partial  bool // files match that the list leaves out, hence the note
}

// scanMaxFiles caps the scan so a walk that never ends cannot eat memory.
// A repo that passes it loses whatever rg listed last, hence the note.
const scanMaxFiles = 50000

// listMaxFiles caps the matches the popup lists: every frame builds the
// whole list, and nobody scrolls past a few hundred paths to find one.
const listMaxFiles = 200

func (a *App) completionEnabled() bool { return a.host.Settings.Bool("ui.completion.files") }

// completionMax is the rows the popup shows, clamped to what a popup can show.
func (a *App) completionMax() int {
	return min(max(a.host.Settings.Int("ui.completion.max_items"), 1), 50)
}

// updateCompletion recomputes the completion state after a composer edit:
// the token is the text between a trigger that starts a word and the
// cursor; anything else closes the popup. Lua sources run on the worker,
// never on the key path. The returned cmd launches a file scan when an "@"
// popup just opened.
func (a *App) updateCompletion() tea.Cmd {
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
		// narrow by prefix, so a fast tab never takes a row the word left
		f.extra = slices.DeleteFunc(f.extra, func(it listItem) bool {
			return !strings.HasPrefix(strings.ToLower(it.label), strings.ToLower(f.token))
		})
		trigger, token, line := string(r[i]), f.token, string(r[:cur])
		a.background(func() {
			items := a.host.Complete(context.Background(), trigger, "", token, line)
			a.do(func() {
				if !f.open || a.composer.cursor > len(a.composer.runes()) || string(a.composer.runes()[:a.composer.cursor]) != line {
					return // the draft moved on
				}
				f.extra = f.extra[:0]
				for _, it := range items {
					f.extra = append(f.extra, listItem{label: it.Text, desc: it.Desc})
				}
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
	a.completion.files = msg.files
	if a.completion.open {
		a.completion.rematch()
	}
}

func (f *fileCompleter) close() {
	f.open = false
	f.token = ""
	f.selected, f.view = 0, viewport{}
	f.matches, f.extra = f.matches[:0], nil
}

func (f *fileCompleter) count() int { return len(f.extra) + len(f.matches) }

// shown reports whether the popup is drawn and takes its keys: a Lua
// trigger with nothing to offer stays out of the way of prose.
func (f *fileCompleter) shown() bool { return f.open && (f.trigger == '@' || f.count() > 0) }

// rematch re-filters and re-ranks the file list against the token.
func (f *fileCompleter) rematch() {
	f.matches = f.matches[:0]
	if f.token == "" {
		f.matches = append(f.matches, f.files...)
	} else {
		type scored struct {
			path  string
			score int
		}
		var cands []scored
		for _, p := range f.files {
			if s, ok := matchScore(p, f.token); ok {
				cands = append(cands, scored{p, s})
			}
		}
		slices.SortFunc(cands, func(x, y scored) int {
			return cmp.Or(cmp.Compare(x.score, y.score), cmp.Compare(len(x.path), len(y.path)), strings.Compare(x.path, y.path))
		})
		for _, c := range cands {
			f.matches = append(f.matches, c.path)
		}
	}
	f.partial = len(f.matches) > listMaxFiles || len(f.files) >= scanMaxFiles
	f.matches = f.matches[:min(len(f.matches), listMaxFiles)]
	f.selected = max(0, min(f.selected, f.count()-1))
}

// matchScore ranks a path against the query: basename prefix beats
// basename subsequence beats path subsequence; lower wins.
func matchScore(path, query string) (int, bool) {
	p := strings.ToLower(path)
	q := strings.ToLower(query)
	base := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		base = p[i+1:]
	}
	switch {
	case strings.HasPrefix(base, q):
		return 0, true
	case subseq(base, q):
		return 1, true
	case subseq(p, q):
		return 2, true
	}
	return 0, false
}

// subseq reports whether q is a subsequence of s.
func subseq(s, q string) bool {
	for len(q) > 0 {
		i := strings.IndexByte(s, q[0])
		if i < 0 {
			return false
		}
		s = s[i+1:]
		q = q[1:]
	}
	return true
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
		text = f.matches[f.selected-n]
	}
	a.composer.replace(f.start+1, text+" ")
	f.close()
}

// completionRows renders the popup rows shown above the composer, at most
// room rows of at most maxW cells.
func (a *App) completionRows(maxW, room int) [][]hostapi.Segment {
	f := &a.completion
	if !f.shown() {
		return nil
	}
	note := func(text string) []hostapi.Segment {
		return []hostapi.Segment{{Text: text, HL: "Host:Hint"}}
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
		items = append(items, listItem{label: m})
	}
	rows := listRows(items, f.selected, &f.view, min(a.completionMax(), room), maxW)
	if f.partial {
		rows = append(rows, partial)
	}
	return rows
}

// scanFilesCmd lists the project's files off the UI goroutine.
func (a *App) scanFilesCmd(dir string, gen int) tea.Cmd {
	return func() tea.Msg {
		files := tools.ListFiles(dir, scanMaxFiles)
		slices.Sort(files)
		return filesScannedMsg{gen: gen, files: files}
	}
}

type filesScannedMsg struct {
	gen   int
	files []string
}
