// find.go searches every project's sessions: the CLI's `zemi sessions search`
// and the TUI's /sessions search share it, so the two never disagree about
// what a query finds. Each project is one read-only query; a store zemi
// cannot read is skipped and named, never fatal, since the answer is a
// session, not an error.

package hostapi

import (
	"cmp"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"unicode"

	"charm.land/fantasy"
	"github.com/sahilm/fuzzy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// FoundHit is one text part a search matched.
type FoundHit struct {
	Role, Text string
	At         int64
}

// FoundSession is a session a search matched, by its text or its title.
type FoundSession struct {
	Root, ID, Title string
	UpdatedAt       int64
	Count           int
	TitleHit        bool
	Missing         bool
	Hits            []FoundHit
}

// Found is what one search over every project answered.
type Found struct {
	Projects int
	Skipped  []string
	Sessions []FoundSession
}

// ProjectSessions is one project's top-level sessions, last used first.
type ProjectSessions struct {
	Root     string
	Sessions []*store.Session
}

// projectRoots are the stores a read of every project opens: this project
// first, then the others last used first. The current root is added even when
// no root file names it, since its store can exist without one.
func projectRoots(dirs config.Dirs) []string {
	root := dirs.TrustRoot()
	roots := []string{root}
	for _, p := range ListProjects(dirs, false) {
		if p.Root != root {
			roots = append(roots, p.Root)
		}
	}
	return roots
}

// searchWords is a query as the words the search takes: punctuation at an
// edge is cut, since it never matches, and a word left empty dropped.
func searchWords(query string) []string {
	var words []string
	for _, w := range strings.Fields(query) {
		w = strings.TrimFunc(w, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if w != "" {
			words = append(words, w)
		}
	}
	return words
}

// FindSessions searches every project's store. A store it cannot read is
// skipped, never fatal: the answer is a session, not an error.
func FindSessions(dirs config.Dirs, query string) Found {
	var out Found
	words := searchWords(query)
	if len(words) == 0 {
		return out
	}
	for _, root := range projectRoots(dirs) {
		out.Projects++
		db, err := store.OpenRead(store.ProjectDBPath(dirs.StateHome, root))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			out.Skipped = append(out.Skipped, root)
			continue
		}
		rows, err := searchStore(db, root, query, words)
		db.Close()
		if err != nil {
			out.Skipped = append(out.Skipped, root)
			continue
		}
		out.Sessions = append(out.Sessions, rows...)
	}
	slices.SortFunc(out.Sessions, func(a, b FoundSession) int {
		return cmp.Or(
			bit(b.TitleHit)-bit(a.TitleHit),
			cmp.Compare(b.Count, a.Count),
			cmp.Compare(b.UpdatedAt, a.UpdatedAt),
			cmp.Compare(a.ID, b.ID),
		)
	})
	return out
}

// bit is a bool as 0 or 1, which cmp.Or can order.
func bit(b bool) int {
	if b {
		return 1
	}
	return 0
}

// searchStore is one project's answer: the sessions a message search matched,
// with a title match added, in store order.
func searchStore(db *store.DB, root, query string, words []string) ([]FoundSession, error) {
	hits, err := db.SearchMessages(words, 5, 4000)
	if err != nil {
		return nil, err
	}
	list, err := db.ListSessions(-1)
	if err != nil {
		return nil, err
	}
	rows := make([]FoundSession, len(list))
	at := make(map[string]int, len(list))
	titles := make([]string, len(list))
	for i, s := range list {
		rows[i] = FoundSession{Root: root, ID: s.ID, Title: s.Title, UpdatedAt: s.UpdatedAt}
		at[s.ID] = i
		titles[i] = s.Title
	}
	matched := make([]bool, len(rows))
	for _, h := range hits {
		i, ok := at[h.SessionID]
		if !ok {
			continue
		}
		rows[i].Count = h.Count
		rows[i].Hits = append(rows[i].Hits, FoundHit{Role: h.Role, Text: snippet(h.Text, h.Role, words[0]), At: h.At})
		matched[i] = true
	}
	// the one subsequence pass: a half-remembered title, not every message
	for _, m := range fuzzy.Find(query, titles) {
		rows[m.Index].TitleHit = true
		matched[m.Index] = true
	}
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		for i := range rows {
			rows[i].Missing = true
		}
	}
	out := make([]FoundSession, 0, len(rows))
	for i, ok := range matched {
		if ok {
			out = append(out, rows[i])
		}
	}
	return out, nil
}

// snippetLen is how much of a hit a row or a detail line shows.
const snippetLen = 180

// snippet is one hit's text as a result shows it: the user's own words, on
// one line, a window around the first match.
func snippet(text, role, word string) string {
	if role == string(fantasy.MessageRoleUser) {
		text, _ = agent.CutAttached(text)
	}
	line := strings.Join(strings.Fields(text), " ")
	runes := []rune(line)
	if len(runes) <= snippetLen {
		return line
	}
	start := 0
	if i, _ := foldIndex(line, word); i >= 0 {
		start = min(max(len([]rune(line[:i]))-snippetLen/2, 0), len(runes)-snippetLen)
	}
	cut := string(runes[start : start+snippetLen])
	if start > 0 {
		cut = "…" + cut
	}
	if start+snippetLen < len(runes) {
		cut += "…"
	}
	return cut
}

// ListAllSessions groups every project's top-level sessions, last used first.
// A store it cannot read is left out: the list is a convenience, not a report.
func ListAllSessions(dirs config.Dirs) []ProjectSessions {
	var out []ProjectSessions
	for _, root := range projectRoots(dirs) {
		db, err := store.OpenRead(store.ProjectDBPath(dirs.StateHome, root))
		if err != nil {
			continue
		}
		list, err := db.ListSessions(-1)
		db.Close()
		if err != nil || len(list) == 0 {
			continue
		}
		out = append(out, ProjectSessions{Root: root, Sessions: list})
	}
	return out
}

// FindSession finds a session by id in this project's store, then in every
// other, for the picker's cross-project open. A store it cannot read is
// skipped: one bad file must not hide a session another project's store has.
func FindSession(dirs config.Dirs, id string) (string, *store.Session, error) {
	for _, root := range projectRoots(dirs) {
		db, err := store.OpenRead(store.ProjectDBPath(dirs.StateHome, root))
		if err != nil {
			continue
		}
		sess, err := db.GetSession(id)
		db.Close()
		if err == nil {
			return root, sess, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			continue
		}
	}
	return "", nil, store.ErrNotFound
}
