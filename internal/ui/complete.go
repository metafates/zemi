package ui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/metafates/zemi/internal/hostapi"
)

// fileCompleter is the composer popup listing project files after the
// trigger character (default "@", DESIGN-style composer completion). The
// file list is scanned off the UI goroutine and fuzzy-ranked client-side.
type fileCompleter struct {
	open     bool
	start    int      // rune index of the trigger char in the composer
	token    string   // query between the trigger and the cursor
	files    []string // paths relative to the session cwd
	matches  []string
	selected int

	scanning bool
	walkGen  int // bumps so stale scan results are dropped
}

var scanSkipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true,
	"vendor": true, "target": true, "dist": true, "build": true,
	".venv": true, "venv": true, "__pycache__": true, ".cache": true,
	"coverage": true, ".idea": true, ".vscode": true,
}

const scanMaxFiles = 5000

// completeTrigger reads the configured trigger character; a config value
// that is not exactly one rune falls back to "@".
func (a *App) completeTrigger() string {
	if v, ok := a.host.Settings.Get("ui.completion.trigger"); ok {
		if s, ok := v.(string); ok && len([]rune(s)) == 1 {
			return s
		}
	}
	return "@"
}

func (a *App) completionEnabled() bool { return a.cfgBool("ui.completion.files", true) }

// updateCompletion recomputes the completion state after a composer edit:
// the token is the text between a trigger char that starts a word and the
// cursor; anything else closes the popup. The returned cmd launches the
// initial file scan when the popup just opened.
func (a *App) updateCompletion() tea.Cmd {
	if !a.completionEnabled() || a.mode != "insert" {
		a.completion.close()
		return nil
	}
	r := a.composer.runes()
	cur := a.composer.cursor
	trigger := a.completeTrigger()
	i := cur
	for i > 0 && r[i-1] != '\n' && r[i-1] != ' ' && r[i-1] != '\t' {
		i--
	}
	switch {
	case i == cur:
		// empty word at the cursor: open only right after the trigger
		if cur == 0 || string(r[cur-1]) != trigger {
			a.completion.close()
			return nil
		}
		a.completion.start = cur - 1
		a.completion.token = ""
	case string(r[i]) == trigger:
		a.completion.start = i
		a.completion.token = string(r[i+1 : cur])
	default:
		a.completion.close()
		return nil
	}
	a.completion.open = true
	a.completion.selected = 0
	a.completion.rematch(a.cfgInt("ui.completion.max_items", 10, 1, 50))
	if len(a.completion.files) == 0 && !a.completion.scanning {
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
		a.completion.rematch(a.cfgInt("ui.completion.max_items", 10, 1, 50))
	}
}

func (f *fileCompleter) close() {
	f.open = false
	f.token = ""
	f.selected = 0
}

// rematch re-filters and re-ranks the file list against the token.
func (f *fileCompleter) rematch(maxItems int) {
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
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].score != cands[j].score {
				return cands[i].score < cands[j].score
			}
			if len(cands[i].path) != len(cands[j].path) {
				return len(cands[i].path) < len(cands[j].path)
			}
			return cands[i].path < cands[j].path
		})
		for _, c := range cands {
			f.matches = append(f.matches, c.path)
		}
	}
	if len(f.matches) > maxItems {
		f.matches = f.matches[:maxItems]
	}
	if f.selected >= len(f.matches) {
		f.selected = len(f.matches) - 1
	}
	if f.selected < 0 {
		f.selected = 0
	}
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

// accept replaces the trigger+token range with the selected match (plus a
// trailing space) and closes the popup.
func (a *App) acceptCompletion() {
	f := &a.completion
	if !f.open || f.selected >= len(f.matches) {
		return
	}
	r := a.composer.runes()
	cur := a.composer.cursor
	repl := f.matches[f.selected] + " "
	a.composer.text = string(r[:f.start]) + repl + string(r[cur:])
	a.composer.cursor = f.start + len([]rune(repl))
	f.close()
}

// completionRows renders the popup rows shown above the composer.
func (a *App) completionRows(w int) [][]hostapi.Segment {
	f := &a.completion
	if !f.open {
		return nil
	}
	if f.scanning && len(f.files) == 0 {
		return [][]hostapi.Segment{{{Text: "  scanning files...", HL: "Comment"}}}
	}
	if len(f.matches) == 0 {
		return [][]hostapi.Segment{{{Text: "  no matches", HL: "Comment"}}}
	}
	var out [][]hostapi.Segment
	for i, m := range f.matches {
		marker, hl := "  ", "Normal"
		if i == f.selected {
			marker, hl = "> ", "Title"
		}
		out = append(out, []hostapi.Segment{
			{Text: " " + marker, HL: hl},
			{Text: ansiTrunc(m, w-6), HL: hl},
		})
	}
	return out
}

// scanFilesCmd walks the directory tree off the UI goroutine.
func (a *App) scanFilesCmd(dir string, gen int) tea.Cmd {
	return func() tea.Msg {
		var files []string
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != dir && (scanSkipDirs[d.Name()] || (strings.HasPrefix(d.Name(), ".") && d.Name() != ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return nil
			}
			files = append(files, rel)
			if len(files) >= scanMaxFiles {
				return filepath.SkipAll
			}
			return nil
		})
		sort.Strings(files)
		return filesScannedMsg{gen: gen, files: files}
	}
}

// ansiTrunc clips a plain string to w cells (no ANSI in file paths).
func ansiTrunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:max0(w-1)]) + "~"
}

type filesScannedMsg struct {
	gen   int
	files []string
}
