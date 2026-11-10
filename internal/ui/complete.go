package ui

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// _sections is the popup's heading per @ kind: the builders name their rows'
// section from here, and an entity row carries the kind so its accept needs
// no second mapping.
var _sections = map[string]string{
	hostapi.AttachFile:     "Files",
	hostapi.AttachSkill:    "Skills",
	hostapi.AttachWorkflow: "Workflows",
	hostapi.AttachAgent:    "Agents",
}

// wordCompleter holds the composer popup that completes the word after a
// trigger: "@" lists the project's files, the skills, the workflows and
// agents - sections under headers - and, for a path-headed word, the
// entries of the directory it names; any trigger lists what its Lua
// sources offer (extra, first).
type wordCompleter struct {
	open       bool
	trigger    rune
	start      int      // rune index of the trigger char in the composer
	token      string   // query between the trigger and the cursor, kind prefix included
	files      []string // paths relative to the session cwd
	bases      []string // each file's last element
	matches    []hostapi.Hit
	skills     []hostapi.CompletionItem // fetched once per open
	skillRows  []listItem               // the skills narrowed to the token
	entities   []listItem               // workflows and agents fetched once per open, each row its section
	entityRows []listItem               // the entities narrowed to the token
	paths      []listItem               // directory candidates: rows of the words they insert
	extra      []listItem               // the Lua sources' items, ahead of the sections
	cmdRows    []listItem               // the commands a draft starting with "/" matches
	argRows    []listItem               // the command's argument candidates, past "/name "
	argStart   int                      // rune index the argument under the cursor starts at
	query      string                   // what the argument list was narrowed by
	moved      bool                     // the user moved the selection in this list
	selected   int
	view       viewport
	size       boxSize // the last drawn size, kept for a frame with no matches to show

	scanning   bool
	walkGen    int      // bumps so stale scan results are dropped
	dirDir     string   // the directory the listing holds, spelled out
	dirList    []string // its entries, "/"-suffixed for directories
	dirPending string   // the directory a listing fetch is in flight for
}

// _scanMaxFiles caps the scan so a walk that never ends cannot eat memory.
const _scanMaxFiles = 50000

// _dirMaxFiles caps one directory's share of the scan, so a directory of
// small files cannot eat the whole corpus on its own.
const _dirMaxFiles = 2000

// _listMaxFiles caps the matches and the directory candidates the popup
// lists: every frame builds the whole list, and nobody scrolls past a few
// hundred paths to find one.
const _listMaxFiles = 200

// _dirListWait bounds a directory listing, the bound git's file listing
// takes too (tools.listWait, fs_tools.go:1045).
const _dirListWait = 2 * time.Second

const _completionMaxRows = 10

// updateCompletion recomputes the composer's popup after an edit: a draft
// starting with "/" lists commands and then their arguments, any other word
// a trigger starts ("@") lists what its sections and Lua sources offer.
// Anything else closes it. Lua sources run on the worker, never on the key
// path. The returned cmd launches a file scan when an "@" popup just opened.
func (a *App) updateCompletion() tea.Cmd {
	r := a.composer.runes()
	cur := a.composer.cursor
	if len(r) > 0 && r[0] == '/' {
		return a.updateSlash()
	}
	i := wordStart(r, cur)
	files := i < cur && r[i] == '@'
	lua := i < cur && a.host.HasCompletions(string(r[i]), "")
	if !files && !lua {
		a.completion.close()
		return nil
	}
	return a.updateWord(r, i, cur, lua)
}

// updateSlash recomputes the popup for a draft starting with "/": while the
// cursor sits in the first word it lists the commands the typed name
// matches, past it the command's argument candidates. The list never holds
// the line back: an unknown name submits as a message, a pasted path among
// the things that ride it.
func (a *App) updateSlash() tea.Cmd {
	f := &a.completion
	r := a.composer.runes()
	cur := a.composer.cursor
	nameEnd := 1
	for nameEnd < len(r) && r[nameEnd] != ' ' && r[nameEnd] != '\n' {
		nameEnd++
	}
	if !f.open || f.trigger != '/' {
		f.close()
	}
	f.trigger, f.start, f.open = '/', 0, true
	if cur <= nameEnd {
		query, args := string(r[1:nameEnd]), strings.TrimSpace(string(r[nameEnd:])) != ""
		f.cmdRows, f.argRows, f.argStart = f.cmdRows[:0], nil, 0
		cmds := a.host.Commands.List()
		if string(r) == "/reload" { // enter reloads, so no row may claim it
			cmds = nil
		}
		names := make([]string, len(cmds))
		for i, c := range cmds {
			names[i] = c.Name
		}
		hits := hostapi.Match(query, names, nil)
		if query != "" {
			hits = hostapi.Rank(hits, names, len(hits))
		}
		for _, h := range hits {
			// hidden commands stay out of the list until their name is typed;
			// with arguments after the name only the names it begins list,
			// since a fuzzy hit would rewrite a line the user may have meant
			// as typed
			if (!cmds[h.Index].Hidden || h.Prefix) && (!args || h.Prefix || query == "") {
				pos := make([]int, len(h.Pos))
				for i, at := range h.Pos {
					pos[i] = at + 1 // past the "/"
				}
				f.cmdRows = append(f.cmdRows, listItem{label: "/" + cmds[h.Index].Name, desc: cmds[h.Index].Desc, match: pos})
			}
		}
		return nil
	}
	// Past "/name ": the argument candidates under the cursor, from the
	// command's Go or Lua completer and its completion sources, matched and
	// ranked in one place (CompleteCommand) before the cap.
	start := cur
	for start > nameEnd && r[start-1] != ' ' {
		start--
	}
	name, typed, line := string(r[1:nameEnd]), string(r[start:cur]), string(r[:cur])
	show := func(cands []listItem, narrow bool) {
		f.cmdRows = nil
		if f.argRows == nil || f.argStart != start { // a new list: the name's gave way, or another argument's
			f.selected, f.moved, f.view = 0, false, viewport{}
		} else if f.query != typed {
			f.selected, f.moved = 0, false
		}
		f.query, f.argStart = typed, start
		f.argRows = matchRows(cands, typed, narrow)
		f.selected = max(0, min(f.selected, len(f.argRows)-1))
	}
	lua := a.host.HasCompletions("", name)
	if c := a.host.Commands.Lookup(name); !lua && (c == nil || c.Complete == nil || c.GoComplete != nil) {
		show(candRows(a.host.CompleteCommand(context.Background(), name, typed, line), ""), false)
		return nil
	}
	// A Lua completer or source runs on the worker, since the key loop never
	// waits on Lua; its answer is dropped once the line before the cursor
	// moved on. Until it lands, the rows it gave narrow by the typed word.
	show(f.argRows, true)
	a.background(func() {
		cands := a.host.CompleteCommand(context.Background(), name, typed, line)
		if lua {
			cands = append(cands, a.host.Complete(context.Background(), "", name, typed, line)...)
		}
		a.do(func() {
			if !f.open || f.trigger != '/' || a.composer.cursor > len(a.composer.runes()) || string(a.composer.runes()[:a.composer.cursor]) != line {
				return // the draft moved on
			}
			show(candRows(cands, ""), false)
		})
	})
	return nil
}

// updateWord recomputes the completion for the word a trigger starts at i.
func (a *App) updateWord(r []rune, i, cur int, lua bool) tea.Cmd {
	f := &a.completion
	opening := !f.open || f.trigger != r[i]
	if opening {
		f.close()
	}
	f.start, f.trigger, f.open, f.selected = i, r[i], true, 0
	f.token = string(r[i+1 : cur])
	kind, rest := hostapi.SplitKind(f.token)
	if lua {
		// until the sources answer, the rows they gave for a shorter word
		// narrow, so a fast tab never takes a row the word left
		f.extra = matchRows(f.extra, unquoteToken(rest), true)
		f.trim()
		trigger, token, line := string(r[i]), f.token, string(r[:cur])
		a.background(func() {
			items := a.host.Complete(context.Background(), trigger, "", token, line)
			a.do(func() {
				if !f.open || a.composer.cursor > len(a.composer.runes()) || string(a.composer.runes()[:a.composer.cursor]) != line {
					return // the draft moved on
				}
				_, rest := hostapi.SplitKind(token)
				f.extra = matchRows(candRows(items, ""), unquoteToken(rest), false) // shown as the sources matched them
				f.trim()
			})
		})
	}
	files := i < cur && r[i] == '@'
	if !files {
		return nil
	}
	if opening {
		// the rows are fetched once per open, like the file scan; typing
		// narrows what the last fetch gave
		f.skills, f.skillRows, f.entities, f.entityRows = nil, nil, nil, nil
		a.background(func() {
			items := a.host.SkillMentions()
			entities := append(candRows(a.host.WorkflowMentions(), hostapi.AttachWorkflow), candRows(a.host.AgentMentions(), hostapi.AttachAgent)...)
			a.do(func() {
				if !f.open || f.trigger != '@' {
					return // the popup closed, or another trigger took it
				}
				f.skills = items
				f.entities = entities
				f.reskill()
				f.rentities()
			})
		})
	}
	f.reskill()
	f.rentities()
	f.rematch()
	a.repaths()
	// a kind other than file takes no paths: its filter leaves no row a
	// listing could fill
	if kind == "" || kind == hostapi.AttachFile {
		if _, _, ok := pathToken(rest); ok {
			if want := a.resolveDir(rest); want != f.dirDir {
				a.listDir(want)
			}
		}
	}
	// Rescan on every open so files created since show up; the old list
	// stays until the new one lands. A path-headed token asks the listing,
	// not the corpus, so the scan waits for a word that is a name - asked
	// by the path it holds, quote frame aside. The corpus still feeds the
	// ambiguity count under any filter.
	if opening && !pathHeaded(unquoteToken(rest)) {
		a.completion.scanning = true
		a.completion.walkGen++
		return a.scanFilesCmd(a.host.Session().CWD, a.completion.walkGen)
	}
	return nil
}

// quoteOpens reports whether a quote at r[i] opens the @ word's frame: it
// stands right after the @, or after the word's kind prefix - @file:"my doc -
// the two places the attach grammar reads a frame (attachRef).
func quoteOpens(r []rune, start, i int) bool {
	if i > 0 && r[i-1] == '@' {
		return true
	}
	if i <= start || r[start] != '@' {
		return false
	}
	kind, _ := hostapi.SplitKind(string(r[start+1 : i]))
	return kind != ""
}

// closeQuotedWord ends the word a space is about to: an @ word holding an
// open quote whose path already resolves gets its closing quote, so prose
// after the space rides outside the word instead of inside the path the
// grammar reads to the line's end. A word still mid-typing - `@"My Doc`
// gaining the space that is part of its name - resolves to nothing and
// keeps its quote open, and so does a bare-head spelling the popup narrows
// (`~Downloads`): shells do not resolve one either, completing the row is
// what lands the canonical form.
func (a *App) closeQuotedWord() {
	c := &a.composer
	r := c.runes()
	i := wordStart(r, c.cursor)
	if i+1 >= c.cursor || r[i] != '@' {
		return
	}
	token := string(r[i+1 : c.cursor])
	_, rest := hostapi.SplitKind(token)
	if rest == "" {
		return
	}
	q := rest[0]
	if q != '"' && q != '\'' {
		return
	}
	if strings.ContainsRune(rest[1:], rune(q)) {
		return // the pair is closed already
	}
	if c.cursor < len(r) && r[c.cursor] != '\n' {
		return // a mid-word cursor would split the word
	}
	w := unquoteToken(rest)
	if w == "" {
		return
	}
	realPath, err := a.host.ResolveWord(w)
	if err != nil {
		return
	}
	if _, err := os.Stat(realPath.String()); err != nil {
		return
	}
	c.insertFits(string(q))
}

// wordStart is the rune index a completion word starts at: after the last
// whitespace outside a quoted run, a run between matched quotes being one
// word, so a quoted token holds its spaces. A quote opens a run only right
// after the `@` trigger, the one place the attach grammar reads a frame
// (attachRef); a quote anywhere else is an ordinary character, so prose
// apostrophes (`don't`) and stray quotes never swallow the rest of the
// line the way a shell's unclosed quote would.
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
		case (c == '"' || c == '\'') && quoteOpens(r, start, i):
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
		f.bases[i] = p[strings.LastIndexByte(strings.TrimSuffix(p, "/"), '/')+1:]
	}
}

func (f *wordCompleter) close() {
	f.open = false
	f.token = ""
	f.selected, f.view, f.size = 0, viewport{}, boxSize{}
	f.matches, f.extra = f.matches[:0], nil
	f.skillRows, f.entityRows, f.paths = nil, nil, nil
	f.entities = nil
	f.cmdRows, f.argRows, f.argStart, f.query, f.moved = nil, nil, 0, "", false
	f.dirDir, f.dirList, f.dirPending = "", nil, ""
}

func (f *wordCompleter) count() int {
	n := len(f.skillRows) + len(f.entityRows) + len(f.paths) + len(f.matches) + len(f.cmdRows) + len(f.argRows)
	if kind, _ := hostapi.SplitKind(f.token); kind == "" { // a kind filter is not a Lua source's
		n += len(f.extra)
	}
	return n
}

// shown reports whether the popup is drawn and takes its keys: a Lua
// trigger with nothing to offer stays out of the way of prose, while the
// composer's own triggers draw their empty list ("No matches.") so the
// reader sees why nothing picks.
func (f *wordCompleter) shown() bool {
	return f.open && (f.trigger == '@' || f.trigger == '/' || f.count() > 0)
}

// title names what the popup lists for the triggers zemi knows the meaning
// of; a Lua source's own trigger names nothing here.
func (f *wordCompleter) title() string {
	switch f.trigger {
	case '@':
		return "Attach"
	case '/':
		return "Commands"
	}
	return ""
}

// reskill narrows the fetched skills to the token; a kind filter that is
// not the skills' leaves them no rows.
func (f *wordCompleter) reskill() {
	kind, rest := hostapi.SplitKind(f.token)
	if kind != "" && kind != hostapi.AttachSkill {
		f.skillRows = nil
		f.trim()
		return
	}
	f.skillRows = matchRows(candRows(f.skills, hostapi.AttachSkill), unquoteToken(rest), true)
	f.trim()
}

// rentities narrows the fetched workflows and agents to the token; a kind
// filter keeps only its own rows, and the file and skill kinds none.
func (f *wordCompleter) rentities() {
	kind, rest := hostapi.SplitKind(f.token)
	f.entityRows = matchRows(f.entities, unquoteToken(rest), true)
	if kind != "" {
		f.entityRows = slices.DeleteFunc(f.entityRows, func(r listItem) bool { return r.kind != kind })
	}
	f.trim()
}

// pathToken splits a path-headed token into the directory the listing
// answers, as typed without its quotes and always ending in its
// separator, and the entry base the cursor narrows. A head typed without
// its separator - `~Downloads`, `..Docs` - still heads one: its letters
// narrow the head's listing, like the letters after a slash narrow the
// last one.
func pathToken(token string) (dir, base string, ok bool) {
	w := unquoteToken(token)
	if !pathHeaded(w) {
		return "", "", false
	}
	if i := strings.LastIndexByte(w, '/'); i > 0 {
		return w[:i+1], w[i+1:], true
	}
	switch {
	case strings.HasPrefix(w, "~"):
		return "~/", w[1:], true
	case strings.HasPrefix(w, ".."):
		return "../", w[2:], true
	default:
		return "/", w[1:], true
	}
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
// where a bare name is one of the corpus's files. A head spelled without
// its separator - `~x`, `..x` - names one too; so does a word ending in
// the separator - `internal/` - whatever frame it rides. `.` alone does
// not, so a hidden name still answers from the corpus.
func pathHeaded(w string) bool {
	return strings.HasPrefix(w, "~") || strings.HasPrefix(w, "..") ||
		strings.HasPrefix(w, "/") || strings.HasPrefix(w, "./") ||
		strings.HasSuffix(w, "/")
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
	f := &a.completion
	f.dirPending = dir
	a.background(func() {
		entries := readDir(dir)
		a.do(func() {
			f := &a.completion
			if f.dirPending == dir {
				f.dirPending = ""
			}
			if !f.open || f.trigger != '@' || a.resolveDir(f.token) != dir {
				return // the popup closed, or the token walked somewhere else
			}
			f.dirDir, f.dirList = dir, entries
			a.repaths()
		})
	})
}

// readDir lists one directory for the path candidates: entry names with a
// trailing slash on directories, directories first then lexical. The read is
// whole up to scanMaxFiles - the word's base narrows what the directory
// holds, and the display caps live in repaths - so a file past a display cap
// stays reachable by name. A mount that answers slowly holds the worker at
// most dirListWait; the abandoned read's answer drops.
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
			if de.IsDir() {
				if n++; n > _scanMaxFiles {
					break // NOTE: a directory past the scan ceiling narrows its first scanMaxFiles entries
				}
				out = append(out, de.Name()+"/")
			}
		}
		n = 0
		for _, de := range des {
			if !de.IsDir() {
				if n++; n > _scanMaxFiles {
					break
				}
				out = append(out, de.Name())
			}
		}
		ch <- out
	}()
	select {
	case out := <-ch:
		return out
	case <-time.After(_dirListWait):
		return nil
	}
}

// repaths rebuilds the directory candidates for a path-headed token from
// the listing the cache holds. It runs on every keystroke: a word that
// stops being a path drops the rows a path left behind. The token's base
// narrows the entries fuzzily, like the corpus's files, and a candidate
// joins the token's directory as typed, quote included, so the user's
// frame survives the walk.
func (a *App) repaths() {
	f := &a.completion
	f.paths = f.paths[:0]
	kind, rest := hostapi.SplitKind(f.token)
	if kind != "" && kind != hostapi.AttachFile {
		return
	}
	dir, base, ok := pathToken(rest)
	if !ok || a.resolveDir(rest) != f.dirDir {
		return
	}
	q := ""
	if t := rest; t != "" && (t[0] == '"' || t[0] == '\'') {
		q = string(t[0])
	}
	hits := hostapi.Match(base, f.dirList, nil)
	// a word typed ranks the candidates by score, like the Files section;
	// a bare listing keeps the directory's own order
	if base != "" {
		hits = hostapi.Rank(hits, f.dirList, _listMaxFiles)
	}
	// each kind keeps half the list, so a directory dense in one kind
	// cannot hide the other from the bare listing
	var kept [2]int
	for _, h := range hits {
		k := 0
		if strings.HasSuffix(f.dirList[h.Index], "/") {
			k = 1
		}
		if kept[k] >= _listMaxFiles/2 {
			continue
		}
		kept[k]++
		word := q + dir + f.dirList[h.Index]
		for kk := range h.Pos {
			h.Pos[kk] += len(q) + len(dir)
		}
		f.paths = append(f.paths, listItem{label: word, match: h.Pos, score: pickScore(h), section: _sections[hostapi.AttachFile], row: hostapi.OverlayItem{Tail: true}})
	}
	f.trim()
}

// rematch ranks the files the token matches, a file name it begins first.
// A name it matches scores by the name alone, which a path's other elements
// would only blur; a file matched across directories scores by its path. A
// path-headed token asks the directory listing, not the corpus.
func (f *wordCompleter) rematch() {
	f.matches = f.matches[:0]
	kind, rest := hostapi.SplitKind(f.token)
	if kind != "" && kind != hostapi.AttachFile {
		f.trim()
		return
	}
	if _, _, ok := pathToken(rest); !ok {
		if rest == "" {
			f.matches = hostapi.Match("", f.files[:min(len(f.files), _listMaxFiles)], nil)
		} else {
			// chunks of the tree match at once: scoring the thousands of hits
			// a short token has in a big tree takes longer than a keystroke
			// may on one core
			const chunk = 8192
			best := make([][]hostapi.Hit, (len(f.files)+chunk-1)/chunk)
			var wg sync.WaitGroup
			for c := range best {
				wg.Go(func() {
					best[c] = f.matchFiles(c*chunk, min(c*chunk+chunk, len(f.files)), rest)
				})
			}
			wg.Wait()
			f.matches = hostapi.Rank(slices.Concat(best...), f.files, _listMaxFiles)
		}
	}
	f.trim()
}

// matchFiles matches the token against files[lo:hi]: the best listMaxFiles
// hits, indexed into files.
func (f *wordCompleter) matchFiles(lo, hi int, token string) []hostapi.Hit {
	files, bases := f.files[lo:hi], f.bases[lo:hi]
	// a quoted token narrows by its path: the quote is the word's frame,
	// not part of the name it asks for
	token = unquoteToken(token)
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
	return hostapi.Rank(hits, f.files, _listMaxFiles)
}

// candRows are completion candidates as list rows of kind: its section, which
// the accept spells the kind for. A path keeps its tail when cut, where its
// name is.
func candRows(cands []hostapi.CompletionItem, kind string) []listItem {
	rows := make([]listItem, 0, len(cands))
	for _, c := range cands {
		rows = append(rows, listItem{label: c.Text, desc: c.Desc, section: _sections[kind], kind: kind, row: hostapi.OverlayItem{Tail: strings.Contains(c.Text, "/")}})
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
		r.match, r.score = nil, 0
		if len(hits) > 0 && hits[0].Index == i {
			r.match, r.score, hits = hits[0].Pos, pickScore(hits[0]), hits[1:]
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
// word in, shell-tab style. A skill, workflow, agent or Lua-source row
// keeps the trigger, which is its own; a project file becomes the host's
// @ word for it. A name another kind also answers is inserted with its
// kind spelled out - @file:review - the word the expander forces; a name
// one kind holds rides alone.
func (a *App) acceptCompletion() tea.Cmd {
	f := &a.completion
	if !f.open || f.selected >= f.count() {
		return nil
	}
	if f.trigger == '/' {
		// a name fills "/name " and asks what comes next; an argument
		// replaces the one under the cursor
		if f.cmdRows != nil {
			a.composer.text = f.cmdRows[f.selected].label + " "
			a.composer.moveEnd()
			f.close()
			return a.updateCompletion()
		}
		a.composer.replace(f.argStart, f.argRows[f.selected].label+" ")
		f.close()
		return nil
	}
	n, mt, p, s := 0, len(f.matches), len(f.paths), len(f.skillRows)
	kind, rest := hostapi.SplitKind(f.token)
	if kind == "" { // a kind filter hides the Lua sources' rows
		n = len(f.extra)
	}
	// a typed kind prefix rides every word an accept builds, so the popup
	// keeps filtering as the walk goes on
	pre := ""
	if kind != "" {
		pre = kind + ":"
	}
	switch sel := f.selected; {
	case sel < n: // a Lua source's row: its trigger is its own
		a.composer.replace(f.start+1, f.extra[sel].label+" ")
		f.close()
	case sel < n+mt:
		rel := f.files[f.matches[sel-n].Index]
		if strings.HasSuffix(rel, "/") { // a directory walks in
			word := rel
			q := byte(0)
			if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
				q = rest[0] // the frame the word rode in on stays
			} else if !hostapi.CanRef(word) {
				q = '"'
			}
			if q != 0 {
				if strings.ContainsRune(word, rune(q)) {
					q = hostapi.FrameQuote(word)
				}
				word = string(q) + word
			}
			a.composer.replace(f.start+1, pre+word)
			return a.updateCompletion()
		}
		ins := strings.TrimPrefix(a.host.AttachWord(filepath.Join(a.host.Session().CWD, rel)), "@")
		// a bare name another kind answers spells its kind out, the word
		// the expander forces, in place of the host's ./ escape
		if !strings.ContainsAny(rel, `/\`) && f.claims(rel) > 1 {
			ins = hostapi.AttachFile + ":" + rel
			if !hostapi.CanRef(ins) {
				q := hostapi.FrameQuote(rel)
				ins = hostapi.AttachFile + ":" + string(q) + rel + string(q)
			}
		}
		a.composer.replace(f.start, "@"+ins+" ")
		f.close()
	case sel < n+mt+p:
		word := f.paths[sel-n-mt].label
		if strings.HasSuffix(word, "/") { // a directory walks in
			body, q := word, byte(0)
			if c := word[0]; c == '"' || c == '\'' {
				body, q = word[1:], c // the frame the word rode in on stays
			} else if !hostapi.CanRef(word) {
				// a word a bare token cannot carry rides quoted, the quote
				// left open: the letters after it keep narrowing, and a
				// file accept closes the pair
				q = '"'
			}
			if q != 0 {
				if strings.ContainsRune(body, rune(q)) {
					q = hostapi.FrameQuote(body)
				}
				word = string(q) + body
			}
			a.composer.replace(f.start+1, pre+word)
			return a.updateCompletion()
		}
		body, q := word, byte(0)
		if c := word[0]; c == '"' || c == '\'' {
			body, q = word[1:], c
		}
		if hostapi.CanRef(body) {
			word = body // rides bare, nothing to close
		} else {
			if q == 0 || strings.ContainsRune(body, rune(q)) {
				q = hostapi.FrameQuote(body)
			}
			word = string(q) + body + string(q) // a file closes the pair a directory leaves open
		}
		a.composer.replace(f.start+1, pre+word+" ")
		f.close()
	case sel < n+mt+p+s:
		a.composer.replace(f.start+1, f.kindWord(f.skillRows[sel-n-mt-p].kind, f.skillRows[sel-n-mt-p].label)+" ")
		f.close()
	default:
		row := f.entityRows[sel-n-mt-p-s]
		a.composer.replace(f.start+1, f.kindWord(row.kind, row.label)+" ")
		f.close()
	}
	return nil
}

// claims counts the kinds that answer a bare name: the file corpus, the
// skills, and the workflows and agents each on their own.
func (f *wordCompleter) claims(name string) int {
	n := 0
	if slices.Contains(f.bases, name) {
		n++
	}
	if slices.ContainsFunc(f.skills, func(s hostapi.CompletionItem) bool { return s.Text == name }) {
		n++
	}
	for _, r := range f.entities {
		if r.label == name {
			n++
		}
	}
	return n
}

// kindWord is what an accept inserts for a row of kind: the name alone when
// no other kind answers it, else kind:name - the word the expander reads as
// forced to this kind.
func (f *wordCompleter) kindWord(kind, name string) string {
	if f.claims(name) > 1 {
		return kind + ":" + name
	}
	return name
}

// fileCap is the rows the Files section may take while the Skills section
// has rows too, so both headers stay in the popup's window: the Files
// header, the blank separator before the Skills one, the Skills header and
// the skill row under it price in, and a Lua source's rows price in with
// their blank. No skill matches, no cap: narrowing does the work.
func (f *wordCompleter) fileCap() int {
	if len(f.skillRows) == 0 {
		return _listMaxFiles
	}
	n := _completionMaxRows - 4
	if len(f.extra) > 0 {
		n -= len(f.extra) + 1
	}
	return max(n, 0)
}

// pickScore is a matched row's number in the popup's preselection: a prefix
// match leads any subsequence, the same lead Rank gives the Files section.
func pickScore(h hostapi.Hit) int {
	if h.Prefix {
		return h.Score + 1<<30
	}
	return h.Score
}

// pick moves the cursor to the best match among the rows the token kept: the
// sections keep their order, and the cursor names the word's best match - a
// skill it spells, say, over a file it barely matches - so the row tab takes
// is the one the word meant. Every rebuild re-picks: rows that land after
// their fetch change what the index the cursor holds points at.
func (f *wordCompleter) pick() {
	at, best, i := 0, 0, 0
	note := func(score int) {
		if score > best {
			at, best = i, score
		}
		i++
	}
	kind, _ := hostapi.SplitKind(f.token)
	if kind == "" { // a kind filter is not a Lua source's
		for _, r := range f.extra {
			note(r.score)
		}
	}
	for _, m := range f.matches {
		note(pickScore(m))
	}
	for _, r := range f.paths {
		note(r.score)
	}
	for _, r := range f.skillRows {
		note(r.score)
	}
	for _, r := range f.entityRows {
		note(r.score)
	}
	f.selected = max(0, min(at, f.count()-1))
}

// trim clips the file matches and the path rows to the rows fileCap lets
// show, so the selection can only land on a row the popup drew.
func (f *wordCompleter) trim() {
	n := f.fileCap()
	f.matches = f.matches[:min(len(f.matches), n)]
	f.paths = f.paths[:min(len(f.paths), n)]
	f.pick()
}

// completionRows renders the popup's list, at most room rows of at most
// maxW cells, padded to minW.
func (a *App) completionRows(maxW, room, minW int) [][]hostapi.Segment {
	f := &a.completion
	note := func(text string) []hostapi.Segment {
		return []hostapi.Segment{{Text: text, HL: hostapi.ChromeHint}}
	}
	if f.trigger == '/' {
		items := f.cmdRows
		if items == nil {
			items = f.argRows
		}
		if len(items) == 0 {
			return emptyRows(_noMatches, maxW)
		}
		return listRowsW(items, items, f.selected, &f.view, min(_completionMaxRows, room), minW, maxW)
	}
	kind, rest := hostapi.SplitKind(f.token)
	if f.count() == 0 {
		if f.scanning && len(f.files) == 0 && (kind == "" || kind == hostapi.AttachFile) && !pathHeaded(unquoteToken(rest)) {
			return [][]hostapi.Segment{note("scanning files…")}
		}
		if f.dirPending != "" && f.dirPending == a.resolveDir(rest) {
			return [][]hostapi.Segment{note("listing…")}
		}
		return emptyRows(_noMatches, maxW)
	}
	var items []listItem
	if kind == "" { // a kind filter is not a Lua source's
		items = slices.Clone(f.extra)
	}
	items = slices.Grow(items, f.count())
	for _, m := range f.matches {
		items = append(items, listItem{label: f.files[m.Index], match: m.Pos, section: _sections[hostapi.AttachFile], row: hostapi.OverlayItem{Tail: true}})
	}
	items = append(items, f.paths...)
	items = append(items, f.skillRows...)
	items = append(items, f.entityRows...)
	return listRowsW(items, items, f.selected, &f.view, min(_completionMaxRows, room), minW, maxW)
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
// files and the scan at scanMaxFiles, and sorts shallow-first, so the
// empty token's head is the top-level files. Directories ride the list
// too, separator-suffixed: they are attachable, and a dir accept walks in.
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
		isDir := d.IsDir()
		if isDir {
			if path == root {
				return nil
			}
			name := d.Name()
			if name == ".git" || tools.SkipDir(name) || tools.SkipDir(strings.TrimPrefix(name, ".")) {
				return filepath.SkipDir
			}
		} else {
			if !d.Type().IsRegular() {
				return nil
			}
			if dir := path[:strings.LastIndexByte(path, filepath.Separator)+1]; dir != cur {
				cur, n = dir, 0
			}
			n++
			if n > _dirMaxFiles {
				return filepath.SkipDir
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		if isDir {
			rel += "/"
		}
		files = append(files, filepath.ToSlash(rel))
		if len(files) >= _scanMaxFiles {
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

// clearComposer empties the draft and closes the popup. The paste map
// stays: the undo stack still holds the tokens of the draft being cleared,
// and without their text a ctrl+z sends the placeholder to the model.
func (a *App) clearComposer() {
	a.composer.text, a.composer.cursor = "", 0
	a.completion.close()
}

// submitComposer routes the composer submit: a line naming a command runs
// it and leaves the reader where they are, everything else goes through the
// vetoable composer.submit event and the agent (DESIGN §6.2, §6.3) - a "/"
// line that names no command is a message like any other, a pasted path
// among them. A draft the cap refuses (a recalled message, a paste token's
// text) refuses whole, the draft kept for trimming.
func (a *App) submitComposer() {
	text := strings.TrimSpace(a.composer.expand(a.composer.text))
	if text == "" {
		return
	}
	if utf8.RuneCountInString(text) > _maxDraftChars {
		a.draftCapToastNow()
		return
	}
	if raw, ok := strings.CutPrefix(text, "/"); ok {
		if raw == "reload" { // the Go-reserved force reload (DESIGN §4)
			a.clearComposer()
			// off the UI goroutine: replaying the boot may ask for trust
			a.host.GoSafe("reload", a.host.Reload)
			return
		}
		name, args := hostapi.SplitCommand(raw)
		if cmd := a.host.Commands.Lookup(name); cmd != nil {
			a.clearComposer()
			a.runCommand(cmd.Name, args, false)
			return
		}
	}
	a.followStage()
	a.sendMessage(text)
}

// sendMessage runs the composer.submit vetoable event, then appends the
// message to the transcript and drives the agent. A rewrite verdict replaces
// the text the user typed, before it is sent.
func (a *App) sendMessage(text string) {
	res := a.host.EmitVetoable(context.Background(), hostapi.EventComposerSubmit, map[string]any{"text": text})
	if res.Denied {
		a.pushToast(slog.LevelWarn, "Message blocked", res.Reason)
		return
	}
	if res.Text != "" {
		text = res.Text
	}
	draft := a.composer.text
	a.composer.pushHistory(text)
	a.clearComposer()
	if id := a.host.ViewedAgent(); id != "" {
		if a.host.MessageAgent(id, text) == nil {
			return
		}
		_ = a.host.ViewAgent("") // the run is over: messages go to the session, so show it
		a.applyScroll(hostapi.ScrollBottom, _regionNone)
	}
	if err := a.host.AgentSendUser(text); err != nil {
		a.pushToast(slog.LevelError, "Could not send", err.Error())
		a.composer.text = draft
		a.composer.moveEnd()
		a.publishComposer()
	}
}

// restoreQueue moves queued steering messages back into the composer,
// ahead of any draft, so they can be edited and sent again, and returns how
// many it restored. The joined text joins the history: AgentTakeQueue
// consumed it, so history is all that keeps it when an undo takes the draft
// back.
func (a *App) restoreQueue() int {
	q := a.host.AgentTakeQueue()
	if len(q) == 0 {
		return 0
	}
	if !a.composer.empty() {
		q = append(q, a.composer.text)
	}
	text := strings.Join(q, "\n\n")
	a.composer.pushHistory(text)
	a.composer.text = text
	a.composer.moveEnd()
	a.completion.close()
	return len(q)
}
