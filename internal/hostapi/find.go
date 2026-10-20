// find.go searches every project's sessions: the CLI's `zemi sessions search`
// and the TUI's /sessions search share it, so the two never disagree about
// what a query finds. Each project is one read-only query; a store zemi
// cannot read is skipped and named, never fatal, since the answer is a
// session, not an error.

package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"charm.land/fantasy"
	"github.com/sahilm/fuzzy"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
)

// FoundSession is a session a search matched, by its text or its title.
type FoundSession struct {
	Root, ID, Title string
	UpdatedAt       int64
	Count           int
	TitleHit        bool
	Missing         bool
	Hits            []string // snippets, newest first
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
	for w := range strings.FieldsSeq(query) {
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
		out.Projects++ // a store that answered, not a root that has none
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
		rows[i].Hits = append(rows[i].Hits, snippet(h.Text, h.Role, words))
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
func snippet(text, role string, words []string) string {
	if role == string(fantasy.MessageRoleUser) {
		text, _ = agent.CutAttached(text)
	}
	line := sanitize.Line(text)
	runes := []rune(line)
	if len(runes) <= snippetLen {
		return line
	}
	at := -1 // the earliest of the query's words in the text
	for _, w := range words {
		if i, _ := foldIndex(line, w); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	start := 0
	if at >= 0 {
		start = min(max(len([]rune(line[:at]))-snippetLen/2, 0), len(runes)-snippetLen)
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

// SessionProject is the project root whose store holds session id: the
// current one first, then the others last used first, false for none. A
// store that cannot be read is skipped, as it is by a search.
func SessionProject(dirs config.Dirs, id string) (root string, ok bool) {
	if id == "" {
		return "", false
	}
	for _, r := range projectRoots(dirs) {
		db, err := store.OpenRead(store.ProjectDBPath(dirs.StateHome, r))
		if err != nil {
			continue
		}
		_, err = db.GetSession(id)
		db.Close()
		if err == nil {
			return r, true
		}
	}
	return "", false
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

// findID is the one screen a session search opens: the input, the searching
// box and the result are the same overlay.
const findID = "sessions search"

// OpenFindSessions lists the sessions every project's store matched for
// query; an empty query opens the input that takes the words.
func (h *Host) OpenFindSessions(query string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	seq := h.findSeq.Add(1) // any scan still running is stale now
	if query = strings.TrimSpace(query); query == "" {
		ui.OpenOverlay(Overlay{ID: findID, Title: "Search sessions", Origin: lua.OriginBuiltin, Input: true, Run: "sessions search "})
		return nil
	}
	// the scan takes a second on a big state dir, so its box opens at once and
	// the result replaces it; NOTE: no statusline job for a scan that short
	ui.OpenOverlay(h.findOverlay(Found{}, query, true))
	h.GoSafe("sessions search", func() {
		h.landFind(seq, ui, h.findOverlay(FindSessions(h.Dirs, query), query, false))
	})
	return nil
}

// landFind shows a finished search unless a newer one started since tag, so
// an older answer never replaces the box a newer query opened.
func (h *Host) landFind(tag int64, ui UIHooks, o Overlay) {
	if h.findSeq.Load() == tag {
		ui.OpenOverlay(o)
	}
}

// findOverlay is the search on screen: the words being looked for, then one
// row per session that matched, its newest match under the selected one.
func (h *Host) findOverlay(res Found, query string, searching bool) Overlay {
	o := Overlay{ID: findID, Title: "Search sessions", Origin: lua.OriginBuiltin, Wide: true, Refresh: !searching}
	if searching {
		o.Empty = fmt.Sprintf("Searching every project for %q…", query)
		return o
	}
	current := h.AgentSessionID()
	hits := 0
	items := make([]any, 0, len(res.Sessions))
	for _, s := range res.Sessions {
		hits += s.Count
		desc := []string{filepath.Base(s.Root) + " · " + ago(s.UpdatedAt)}
		if s.Count > 0 {
			desc = append(desc, fmt.Sprintf("%d %s", s.Count, Plural(s.Count, "hit", "hits")))
		}
		if s.TitleHit && s.Count == 0 {
			desc = append(desc, "title") // why a session with no matches is here
		}
		item := map[string]any{
			"text":   markedTitle(s.ID, cmp.Or(s.Title, "(untitled)")),
			"desc":   strings.Join(desc, " · "),
			"fields": fieldList("project", perm.TildePath(s.Root), "updated", ago(s.UpdatedAt), "hits", strconv.Itoa(s.Count)),
		}
		if len(s.Hits) > 0 {
			item["preview"] = s.Hits[0]
		}
		detail := make([]string, 0, len(s.Hits)+1)
		switch {
		case s.Missing:
			detail = append(detail, fmt.Sprintf("%s no longer exists, so this conversation cannot be opened; /projects delete drops it from the list.", perm.TildePath(s.Root)))
		case s.ID == current:
			detail = append(detail, "zemi runs this session: /new leaves it, and then it can be found and resumed here.")
		}
		for _, hit := range s.Hits {
			detail = append(detail, "· "+hit)
		}
		item["detail"] = strings.Join(detail, "\n\n")
		if s.ID == current {
			item["key"] = "current"
		} else {
			item["keys"] = []any{map[string]any{"key": "r", "desc": "Resume", "run": "sessions open " + s.ID + " " + s.Root}}
		}
		items = append(items, item)
	}
	o.Items = items
	unreadable := ""
	if len(res.Skipped) > 0 {
		unreadable = fmt.Sprintf(" · %d unreadable", len(res.Skipped))
	}
	if len(res.Sessions) == 0 {
		o.Empty = fmt.Sprintf("No match for %q in %d %s%s.", query, res.Projects, Plural(res.Projects, "project", "projects"), unreadable)
	} else {
		o.Body = fmt.Sprintf("%d %s in %d %s · %d %s searched%s", hits, Plural(hits, "hit", "hits"),
			len(res.Sessions), Plural(len(res.Sessions), "session", "sessions"), res.Projects, Plural(res.Projects, "project", "projects"), unreadable)
	}
	o.Keys = []OverlayKey{{Key: "r", Desc: "resume"}}
	return o
}

// OpenFoundSession reopens a session a search listed, restarting zemi in its
// project when that is another one: every project fact is bound at start.
func (h *Host) OpenFoundSession(root, id string) error {
	if root == "" || id == "" {
		return Usagef("sessions open takes a session id and the project a search listed")
	}
	if id == h.AgentSessionID() {
		return nil
	}
	root = filepath.Clean(perm.ExpandHome(root))
	if !filepath.IsAbs(root) {
		return Usagef("%s is not an absolute path", perm.TildePath(root))
	}
	if perm.CanonPath(root) == perm.CanonPath(h.trustRoot()) {
		return h.ResumeSession(id) // its own wording for a session that is gone
	}
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	p, err := perm.Resolve(root)
	if err != nil {
		return Usagef("%s is not a directory", perm.TildePath(root))
	}
	root = p.String()
	// a row can sit open while its session is deleted, and a restart cannot
	// take that back
	if err := h.foundSession(root, id); err != nil {
		return err
	}
	if h.AgentActive() {
		return errTurnRunning
	}
	if err := h.tasks.stillRunning(func(string) bool { return true }); err != nil {
		return err
	}
	ui.Relaunch(root, id)
	return nil
}

// foundSession checks a row's project still holds the session it names.
func (h *Host) foundSession(root, id string) error {
	db, err := store.OpenRead(store.ProjectDBPath(h.Dirs.StateHome, root))
	if err == nil {
		defer db.Close()
		_, err = db.GetSession(id)
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, store.ErrNotFound) {
		return Usagef("no session %q in %s", id, perm.TildePath(root))
	}
	return err
}
