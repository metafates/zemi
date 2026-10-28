package ui

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// wordCompleter holds the composer popup that completes the word after a
// trigger: "@" lists the project's files, the skills and, for a path-
// headed word, the entries of the directory it names - sections under
// headers; any trigger lists what its Lua sources offer (extra, first).
type wordCompleter struct {
	open      bool
	trigger   rune
	start     int      // rune index of the trigger char in the composer
	token     string   // query between the trigger and the cursor
	files     []string // paths relative to the session cwd
	bases     []string // each file's last element
	matches   []hostapi.Hit
	skills    []hostapi.CompletionItem // fetched once per open
	skillRows []listItem               // the skills narrowed to the token
	paths     []listItem               // directory candidates: rows of the words they insert
	extra     []listItem               // the Lua sources' items, ahead of the sections
	selected  int
	view      viewport
	size      boxSize // the last drawn size, kept for a frame with no matches to show

	scanning bool
	walkGen  int      // bumps so stale scan results are dropped
	dirDir   string   // the directory the listing holds, spelled out
	dirList  []string // its entries, "/"-suffixed for directories
}

// scanMaxFiles caps the scan so a walk that never ends cannot eat memory.
const scanMaxFiles = 50000

// dirMaxFiles caps one directory's share of the scan, so a directory of
// small files cannot eat the whole corpus on its own.
const dirMaxFiles = 2000

// listMaxFiles caps the matches and the directory candidates the popup
// lists: every frame builds the whole list, and nobody scrolls past a few
// hundred paths to find one.
const listMaxFiles = 200

// dirListWait bounds a directory listing, the bound git's file listing
// takes too (tools.listWait, fs_tools.go:1045).
const dirListWait = 2 * time.Second

const completionMaxRows = 10

// updateCompletion recomputes both popups after a composer edit: the
// palette, and the completion whose token is the text between a trigger
// that starts a word and the cursor; anything else closes it. Lua sources
// run on the worker, never on the key path. The returned cmd launches a
// file scan when an "@" popup just opened.
func (a *App) updateCompletion() tea.Cmd {
	a.updatePalette()
	r := a.composer.runes()
	cur := a.composer.cursor
	i := wordStart(r, cur)
	files := i < cur && r[i] == '@'
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
		f.extra = matchRows(f.extra, unquoteToken(f.token), true)
		trigger, token, line := string(r[i]), f.token, string(r[:cur])
		a.background(func() {
			items := a.host.Complete(context.Background(), trigger, "", token, line)
			a.do(func() {
				if !f.open || a.composer.cursor > len(a.composer.runes()) || string(a.composer.runes()[:a.composer.cursor]) != line {
					return // the draft moved on
				}
				f.extra = matchRows(candRows(items, ""), unquoteToken(token), false) // shown as the sources matched them
				f.trim()
			})
		})
	}
	if !files {
		return nil
	}
	if opening {
		// the rows are fetched once per open, like the file scan; typing
		// narrows what the last fetch gave
		f.skills, f.skillRows = nil, nil
		a.background(func() {
			items := a.host.SkillMentions()
			a.do(func() {
				if !f.open || f.trigger != '@' {
					return // the popup closed, or another trigger took it
				}
				f.skills = items
				f.reskill()
			})
		})
	}
	f.reskill()
	f.rematch()
	if _, _, ok := pathToken(f.token); ok {
		if want := a.resolveDir(f.token); want != f.dirDir {
			a.listDir(want)
		}
		a.repaths()
	}
	// Rescan on every open so files created since show up; the old list
	// stays until the new one lands. A path-headed token asks the listing,
	// not the corpus, so the scan waits for a word that is a name - asked
	// by the path it holds, quote frame aside.
	if opening && !pathHeaded(unquoteToken(f.token)) {
		a.completion.scanning = true
		a.completion.walkGen++
		return a.scanFilesCmd(a.host.Session().CWD, a.completion.walkGen)
	}
	return nil
}

// wordStart is the rune index a completion word starts at: after the last
// whitespace outside a quoted run, a run between matched quotes being one
// word, so a quoted token holds its spaces. A quote opened and never
// closed swallows the rest of the line, as a shell's does.
func wordStart(r []rune, cur int) int {
	line := 0
	for i := cur - 1; i >= 0; i-- {
		if r[i] == '\n' {
			line = i + 1
			break
		}
	}
	var quote rune
	start := line
	for i := line; i < cur; i++ {
		c := r[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case strings.ContainsRune(" \t\n\f\r", c):
			start = i + 1
		}
	}
	return start
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

func (f *wordCompleter) setFiles(files []string) {
	f.files, f.bases = files, make([]string, len(files))
	for i, p := range files {
		f.bases[i] = p[strings.LastIndexByte(p, '/')+1:]
	}
}

func (f *wordCompleter) close() {
	f.open = false
	f.token = ""
	f.selected, f.view, f.size = 0, viewport{}, boxSize{}
	f.matches, f.extra = f.matches[:0], nil
	f.skillRows, f.paths = nil, nil
	f.dirDir, f.dirList = "", nil
}

func (f *wordCompleter) count() int {
	return len(f.extra) + len(f.skillRows) + len(f.paths) + len(f.matches)
}

// shown reports whether the popup is drawn and takes its keys: a Lua
// trigger with nothing to offer stays out of the way of prose.
func (f *wordCompleter) shown() bool { return f.open && (f.trigger == '@' || f.count() > 0) }

// title names what the popup lists for the triggers zemi knows the meaning
// of; a Lua source's own trigger names nothing here.
func (f *wordCompleter) title() string {
	if f.trigger == '@' {
		return "Attach"
	}
	return ""
}

// reskill narrows the fetched skills to the token.
func (f *wordCompleter) reskill() {
	f.skillRows = matchRows(candRows(f.skills, "Skills"), unquoteToken(f.token), true)
	f.trim()
}

// pathToken splits a path-headed token into the directory the listing
// answers, as typed without its quotes and always ending in its
// separator, and the entry base the cursor narrows.
func pathToken(token string) (dir, base string, ok bool) {
	w := unquoteToken(token)
	if !pathHeaded(w) {
		return "", "", false
	}
	if i := strings.LastIndexByte(w, '/'); i >= 0 {
		return w[:i+1], w[i+1:], true
	}
	return w + "/", "", true
}

// unquoteToken strips one wrapping quote pair: the opening quote, and the
// closing one when the word already carries it, so a word still being
// typed answers too.
func unquoteToken(t string) string {
	if t == "" {
		return t
	}
	q := t[0]
	if q != '"' && q != '\'' {
		return t
	}
	t = t[1:]
	if i := strings.LastIndexByte(t, q); i >= 0 {
		t = t[:i]
	}
	return t
}

// pathHeaded reports whether a word names a directory for the listing,
// where a bare name is one of the corpus's files.
func pathHeaded(w string) bool {
	return w == "~" || w == ".." || strings.HasPrefix(w, "~/") ||
		strings.HasPrefix(w, "/") || strings.HasPrefix(w, "../") || strings.HasPrefix(w, "./")
}

// resolveDir spells a token's directory out, where the listing keys its
// cache: ~ under home, / absolute, the rest against the project directory.
func (a *App) resolveDir(token string) string {
	dir, _, ok := pathToken(token)
	if !ok {
		return ""
	}
	if strings.HasPrefix(dir, "~") {
		return perm.ExpandHome(dir)
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Clean(filepath.Join(a.host.Dirs.ProjectDir, dir))
}

// listDir lists dir off the UI goroutine, into the completer's cache.
func (a *App) listDir(dir string) {
	a.background(func() {
		entries := readDir(dir)
		a.do(func() {
			f := &a.completion
			if !f.open || f.trigger != '@' || a.resolveDir(f.token) != dir {
				return // the popup closed, or the token walked somewhere else
			}
			f.dirDir, f.dirList = dir, entries
			a.repaths()
		})
	})
}

// readDir lists one directory for the path candidates: entry names with a
// trailing slash on directories, directories first then lexical, each kind
// capped at listMaxFiles so either alone cannot hide the other. A mount
// that answers slowly holds the worker at most dirListWait; the abandoned
// read's answer drops.
func readDir(dir string) []string {
	ch := make(chan []string, 1)
	go func() {
		des, err := os.ReadDir(dir)
		if err != nil {
			ch <- nil
			return
		}
		out := make([]string, 0, len(des))
		n := 0
		for _, de := range des {
			if de.IsDir() && n < listMaxFiles {
				n++
				out = append(out, de.Name()+"/")
			}
		}
		n = 0
		for _, de := range des {
			if !de.IsDir() && n < listMaxFiles {
				n++
				out = append(out, de.Name())
			}
		}
		ch <- out
	}()
	select {
	case out := <-ch:
		return out
	case <-time.After(dirListWait):
		return nil
	}
}

// repaths rebuilds the directory candidates for a path-headed token from
// the listing the cache holds; a directory it does not hold leaves none.
// The token's base narrows the entries fuzzily, like the corpus's files,
// and a candidate joins the token's directory as typed, quote included,
// so the user's frame survives the walk.
func (a *App) repaths() {
	f := &a.completion
	f.paths = f.paths[:0]
	dir, base, ok := pathToken(f.token)
	if !ok || a.resolveDir(f.token) != f.dirDir {
		return
	}
	q := ""
	if t := f.token; t != "" && (t[0] == '"' || t[0] == '\'') {
		q = string(t[0])
	}
	hits := hostapi.Match(base, f.dirList, nil)
	// a word typed ranks the candidates by score, like the Files section;
	// a bare listing keeps the directory's own order
	if base != "" {
		hits = hostapi.Rank(hits, f.dirList, listMaxFiles)
	}
	// each kind keeps half the list, so a directory dense in one kind
	// cannot hide the other from the bare listing
	var kept [2]int
	for _, h := range hits {
		k := 0
		if strings.HasSuffix(f.dirList[h.Index], "/") {
			k = 1
		}
		if kept[k] >= listMaxFiles/2 {
			continue
		}
		kept[k]++
		word := q + dir + f.dirList[h.Index]
		for kk := range h.Pos {
			h.Pos[kk] += len(q) + len(dir)
		}
		f.paths = append(f.paths, listItem{label: word, match: h.Pos, section: "Files", row: hostapi.OverlayItem{Tail: true}})
	}
	f.trim()
}

// rematch ranks the files the token matches, a file name it begins first.
// A name it matches scores by the name alone, which a path's other elements
// would only blur; a file matched across directories scores by its path. A
// path-headed token asks the directory listing, not the corpus.
func (f *wordCompleter) rematch() {
	f.matches = f.matches[:0]
	if _, _, ok := pathToken(f.token); !ok {
		if f.token == "" {
			f.matches = hostapi.Match("", f.files[:min(len(f.files), listMaxFiles)], nil)
		} else {
			// chunks of the tree match at once: scoring the thousands of hits
			// a short token has in a big tree takes longer than a keystroke
			// may on one core
			const chunk = 8192
			best := make([][]hostapi.Hit, (len(f.files)+chunk-1)/chunk)
			var wg sync.WaitGroup
			for c := range best {
				wg.Go(func() {
					best[c] = f.matchFiles(c*chunk, min(c*chunk+chunk, len(f.files)))
				})
			}
			wg.Wait()
			f.matches = hostapi.Rank(slices.Concat(best...), f.files, listMaxFiles)
		}
	}
	f.trim()
}

// matchFiles matches the token against files[lo:hi]: the best listMaxFiles
// hits, indexed into files.
func (f *wordCompleter) matchFiles(lo, hi int) []hostapi.Hit {
	files, bases := f.files[lo:hi], f.bases[lo:hi]
	// a quoted token narrows by its path: the quote is the word's frame,
	// not part of the name it asks for
	token := unquoteToken(f.token)
	names := hostapi.Match(token, bases, nil)
	rest := slices.Clone(files) // blanked where the name matched: "" matches nothing
	for _, h := range names {
		rest[h.Index] = ""
		for k := range h.Pos {
			h.Pos[k] += len(files[h.Index]) - len(bases[h.Index])
		}
	}
	paths := hostapi.Match(token, rest, nil)
	for i := range paths {
		paths[i].Prefix = false // only a name's prefix leads
	}
	hits := slices.Concat(names, paths)
	for i := range hits {
		hits[i].Index += lo
	}
	return hostapi.Rank(hits, f.files, listMaxFiles)
}

// candRows are completion candidates as list rows under section; a path
// keeps its tail when cut, where its name is.
func candRows(cands []hostapi.CompletionItem, section string) []listItem {
	rows := make([]listItem, 0, len(cands))
	for _, c := range cands {
		rows = append(rows, listItem{label: c.Text, desc: c.Desc, section: section, row: hostapi.OverlayItem{Tail: strings.Contains(c.Text, "/")}})
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

// acceptCompletion replaces what follows the trigger with the selected
// match and closes the popup - unless the row is a directory candidate:
// its word ends in /, so the popup stays open and the listing follows the
// word in, shell-tab style. A skill or Lua-source row keeps the trigger,
// which is its own; a project file becomes the host's @ word for it.
func (a *App) acceptCompletion() {
	f := &a.completion
	if !f.open || f.selected >= f.count() {
		return
	}
	n, mt, p := len(f.extra), len(f.matches), len(f.paths)
	switch sel := f.selected; {
	case sel < n: // a Lua source's row: its trigger is its own
		a.composer.replace(f.start+1, f.extra[sel].label+" ")
		f.close()
	case sel < n+mt:
		rel := f.files[f.matches[sel-n].Index]
		a.composer.replace(f.start, a.host.AttachWord(filepath.Join(a.host.Session().CWD, rel))+" ")
		f.close()
	case sel < n+mt+p:
		word := f.paths[sel-n-mt].label
		if strings.HasSuffix(word, "/") { // a directory walks in
			if q := word[0]; q != '"' && q != '\'' && !hostapi.CanRef(word) {
				word = `"` + word + `"` // a word a bare token cannot carry rides quoted
			}
			a.composer.replace(f.start+1, word)
			a.updateCompletion()
			return
		}
		if q := word[0]; q == '"' || q == '\'' {
			word += string(q) // a file closes the quotes a directory leaves open
		} else if !hostapi.CanRef(word) {
			word = `"` + word + `"`
		}
		a.composer.replace(f.start+1, word+" ")
		f.close()
	default:
		a.composer.replace(f.start+1, f.skillRows[sel-n-mt-p].label+" ")
		f.close()
	}
}

// fileCap is the rows the Files section may take while the Skills section
// has rows too, so both headers stay in the popup's window: the Files
// header, the blank separator before the Skills one, the Skills header and
// the skill row under it price in, and a Lua source's rows price in with
// their blank. No skill matches, no cap: narrowing does the work.
func (f *wordCompleter) fileCap() int {
	if len(f.skillRows) == 0 {
		return listMaxFiles
	}
	n := completionMaxRows - 4
	if len(f.extra) > 0 {
		n -= len(f.extra) + 1
	}
	return max(n, 0)
}

// trim clips the file matches and the path rows to the rows fileCap lets
// show, so the selection can only land on a row the popup drew.
func (f *wordCompleter) trim() {
	n := f.fileCap()
	f.matches = f.matches[:min(len(f.matches), n)]
	f.paths = f.paths[:min(len(f.paths), n)]
	f.selected = max(0, min(f.selected, f.count()-1))
}

// completionRows renders the popup's list, at most room rows of at most
// maxW cells, padded to minW.
func (a *App) completionRows(maxW, room, minW int) [][]hostapi.Segment {
	f := &a.completion
	note := func(text string) []hostapi.Segment {
		return []hostapi.Segment{{Text: text, HL: hostapi.ChromeHint}}
	}
	if f.count() == 0 {
		if f.scanning && len(f.files) == 0 && !pathHeaded(unquoteToken(f.token)) {
			return [][]hostapi.Segment{note("scanning files…")}
		}
		return emptyRows(noMatches, maxW)
	}
	items := slices.Grow(slices.Clone(f.extra), f.count())
	for _, m := range f.matches {
		items = append(items, listItem{label: f.files[m.Index], match: m.Pos, section: "Files", row: hostapi.OverlayItem{Tail: true}})
	}
	items = append(items, f.paths...)
	items = append(items, f.skillRows...)
	return listRowsW(items, items, f.selected, &f.view, min(completionMaxRows, room), minW, maxW)
}

// scanFilesCmd lists the project's files off the UI goroutine.
func (a *App) scanFilesCmd(dir string, gen int) tea.Cmd {
	return func() tea.Msg {
		return filesScannedMsg{gen: gen, files: walkFiles(context.Background(), dir)}
	}
}

// walkFiles lists the project's files for the popup: a real walk, since
// tools.ListFiles is git's list and the popup answers for ignored files
// too. It skips .git and the build directories tools.SkipDir names -
// matched against the name as-is and dot-stripped, so .venv dies with
// venv - keeps every other hidden entry, caps a directory at dirMaxFiles
// entries and the scan at scanMaxFiles, and sorts shallow-first, so the
// empty token's head is the top-level files.
// NOTE: the caps and the name list stand in for a real ignore engine;
// internal/glob's .gitignore reader is the upgrade path.
func walkFiles(ctx context.Context, root string) []string {
	var files []string
	cur, n := "", 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil // an unreadable subdirectory is skipped
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if name == ".git" || tools.SkipDir(name) || tools.SkipDir(strings.TrimPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if dir := path[:strings.LastIndexByte(path, filepath.Separator)+1]; dir != cur {
			cur, n = dir, 0
		}
		n++
		if n > dirMaxFiles {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= scanMaxFiles {
			return filepath.SkipAll
		}
		return nil
	})
	tools.SortShallow(files)
	return files
}

type filesScannedMsg struct {
	gen   int
	files []string
}
