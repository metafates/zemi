package hostapi

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/sahilm/fuzzy"
	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/tools"
)

// CompletionItem is one row a Lua completion source offers.
type CompletionItem struct{ Text, Desc string }

// Hit is a row a filter query matched.
type Hit struct {
	Index  int   // the row
	Pos    []int // byte offsets of the label's matched runes, ascending, for the highlight
	Score  int   // higher is better
	Prefix bool  // the label starts with the query's first word
}

// Match is the one filter of every list a human narrows by typing. It
// returns, in row order, the rows that every word of query matches: a word
// matches the row's label, or appears in its text (texts may be nil). A
// label with a space is prose, which a word must appear in, since a
// subsequence over prose finds nearly every row; a one-token label (a name,
// an id, a path, a dotted key) matches fuzzily. Case never counts. An empty
// query keeps every row.
func Match(query string, labels, texts []string) []Hit {
	words := strings.Fields(query)
	if len(words) == 0 {
		hits := make([]Hit, len(labels))
		for i := range hits {
			hits[i].Index = i
		}
		return hits
	}
	var hits []Hit
	for n, w := range words {
		// the rows still in play, by place: every row for the first word
		count := len(labels)
		if n > 0 {
			count = len(hits)
		}
		row := func(p int) int {
			if n > 0 {
				return hits[p].Index
			}
			return p
		}
		// the fuzzy pass, over the one-token labels the prefilter lets through;
		// other holds the places a substring may still match, in order
		lw := asciiLower(w)
		var tried, other []int
		fz := fuzzy.FindFromIterNoSort(w, func(yield func(string) bool) {
			for p := range count {
				// a label the prefilter rules out holds the word in no form
				l := labels[row(p)]
				in := lw == "" || asciiSubseq(l, lw)
				pr := in && prose(l)
				if pr || texts != nil {
					other = append(other, p)
				}
				if in && !pr {
					tried = append(tried, p)
					if !yield(l) {
						return
					}
				}
			}
		})
		next := make([]Hit, 0, len(fz)+len(other))
		for len(fz) > 0 || len(other) > 0 {
			var p, score int
			var pos []int
			if len(fz) > 0 && (len(other) == 0 || tried[fz[0].Index] <= other[0]) {
				p, pos, score = tried[fz[0].Index], fz[0].MatchedIndexes, fz[0].Score
				if len(other) > 0 && other[0] == p {
					other = other[1:]
				}
				fz = fz[1:]
			} else {
				p, other = other[0], other[1:]
			}
			i := row(p)
			h := Hit{Index: i}
			if n > 0 {
				h = hits[p]
			}
			l := labels[i]
			if pos == nil && prose(l) {
				if s, e := foldIndex(l, w); s >= 0 {
					pos, score = runeStarts(l, s, e, nil), e-s-len(l)
				}
			}
			switch {
			case pos != nil:
				if end := foldPrefix(l, w); end >= 0 {
					// the prefix is what a prefix hit highlights, whichever runes the fuzzy pass took
					pos = runeStarts(l, 0, end, pos[:0])
					h.Prefix = h.Prefix || n == 0
				}
				h.Score += score
				if h.Pos == nil {
					h.Pos = pos
				} else {
					h.Pos = slices.Compact(slices.Sorted(slices.Values(append(h.Pos, pos...))))
				}
			case texts == nil:
				continue
			default:
				if s, _ := foldIndex(texts[i], w); s < 0 {
					continue
				}
			}
			next = append(next, h)
		}
		hits = next
	}
	return hits
}

// Rank keeps the best n hits of a non-empty query, best first, for a popup
// under the composer: prefix hits first, the shortest label leading, then
// the rest by score; ties go to the shorter label, then to row order.
func Rank(hits []Hit, labels []string, n int) []Hit {
	better := func(x, y Hit) int {
		c := 0
		switch {
		case x.Prefix != y.Prefix && x.Prefix:
			c = -1
		case x.Prefix != y.Prefix:
			c = 1
		case !x.Prefix:
			c = cmp.Compare(y.Score, x.Score)
		}
		return cmp.Or(c, cmp.Compare(len(labels[x.Index]), len(labels[y.Index])), cmp.Compare(x.Index, y.Index))
	}
	if len(hits) <= n {
		slices.SortFunc(hits, better)
		return hits
	}
	// a bounded insertion: a one-letter query over a big tree hits most rows
	top := make([]Hit, 0, n+1)
	for _, h := range hits {
		if len(top) == n && better(h, top[n-1]) >= 0 {
			continue
		}
		j, _ := slices.BinarySearchFunc(top, h, better)
		top = slices.Insert(top, j, h)
		top = top[:min(len(top), n)]
	}
	return top
}

// NOTE: a file path with a space in it reads as prose too, so "@mnr" misses
// "docs/My Notes/readme.md"; tell paths apart here if that ever matters.
func prose(label string) bool { return strings.Contains(strings.TrimSpace(label), " ") }

// asciiLower is w lowercased when it is ASCII, else "".
func asciiLower(w string) string {
	for i := range len(w) {
		if w[i] >= utf8.RuneSelf {
			return ""
		}
	}
	return strings.ToLower(w)
}

// asciiSubseq reports whether the lowercase ASCII w is a subsequence of s,
// ignoring case: a byte scan that spares the fuzzy pass most rows.
// NOTE: the two non-ASCII runes that fold to ASCII, the Kelvin sign and the
// long s, never match k or s here; fold per rune if a label ever needs them.
func asciiSubseq(s, w string) bool {
	for i := range len(w) {
		c := w[i]
		j := strings.IndexByte(s, c)
		if 'a' <= c && c <= 'z' {
			end := len(s)
			if j >= 0 {
				end = j // the upper case counts only where it comes first
			}
			if k := strings.IndexByte(s[:end], c-'a'+'A'); k >= 0 {
				j = k
			}
		}
		if j < 0 {
			return false
		}
		s = s[j+1:]
	}
	return true
}

// foldPrefix is the byte length of the prefix of s that equals w ignoring
// case, -1 when s does not start with w.
func foldPrefix(s, w string) int {
	i := 0
	for _, wr := range w {
		r, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 || !equalFold(r, wr) {
			return -1
		}
		i += size
	}
	return i
}

// foldIndex finds w in s ignoring case: the byte span, or -1, -1.
func foldIndex(s, w string) (start, end int) {
	for i := range s {
		if n := foldPrefix(s[i:], w); n >= 0 {
			return i, i + n
		}
	}
	return -1, -1
}

func equalFold(a, b rune) bool {
	for r := unicode.SimpleFold(a); a != b && r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return a == b
}

// runeStarts appends the byte offsets of the runes of s[from:to] to pos.
func runeStarts(s string, from, to int, pos []int) []int {
	for i := range s[from:to] {
		pos = append(pos, from+i)
	}
	return pos
}

// completionSource is a zemi.composer.add registration: it completes
// the word after trigger, or the arguments of command.
type completionSource struct {
	id, trigger, command string
	fn                   *glua.LFunction
	origin               lua.Origin
	path                 string
	gen                  Gen
}

type completeReg struct {
	mu   sync.RWMutex
	list []completionSource
}

func (r *completeReg) purge() {
	r.mu.Lock()
	r.list = nil
	r.mu.Unlock()
}

// _maxCompletions caps what the sources for one popup offer together.
const _maxCompletions = 50

// HasCompletions reports whether a Lua source completes after trigger, or
// the arguments of command, so the UI calls Lua only when one does.
func (h *Host) HasCompletions(trigger, command string) bool {
	return len(h.completions.sources(trigger, command)) > 0
}

// sources is the sources for trigger or command, in registration order.
func (r *completeReg) sources(trigger, command string) []completionSource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.DeleteFunc(slices.Clone(r.list), func(s completionSource) bool {
		return s.trigger != trigger || !strings.EqualFold(s.command, command)
	})
}

// Complete runs the Lua sources for trigger or command, each under its
// origin, and returns their sanitized items in registration order. A
// failing source offers nothing and lands in /errors. It waits on the
// interpreter: never call it on the UI goroutine.
func (h *Host) Complete(ctx context.Context, trigger, command, lead, line string) []CompletionItem {
	srcs := h.completions.sources(trigger, command)
	if len(srcs) == 0 {
		return nil
	}
	var out []CompletionItem
	_, _ = h.RT.Do(ctx, lua.ClassUI, 0, 0, "completion", func(L *glua.LState) ([]any, error) {
		for _, s := range srcs {
			if s.gen != h.currentGen() || len(out) >= _maxCompletions {
				continue
			}
			items, err := h.callCompleter(L, s.fn, luaCaller{gen: s.gen, origin: s.origin, path: s.path}, lead, line)
			if err != nil {
				h.Log.Error("completion source failed", "origin", "ui", "id", s.id, "error", err)
				h.RecordError("completion", s.id, err.Error())
				continue
			}
			out = append(out, items...)
		}
		return nil, nil
	})
	return out[:min(len(out), _maxCompletions)]
}

// callCompleter runs a Lua completer fn(lead, line) under its origin.
func (h *Host) callCompleter(L *glua.LState, fn *glua.LFunction, at luaCaller, lead, line string) ([]CompletionItem, error) {
	rets, err := h.callFn(L, at, fn, 1, glua.LString(lead), glua.LString(line))
	if err != nil {
		return nil, err
	}
	v, err := lua.ToGo(rets[0])
	if err != nil {
		return nil, err
	}
	return completionItems(v), nil
}

// completionItems reads what a completer returned: a list of strings or
// { text, desc } rows, sanitized, rows without text dropped.
func completionItems(v any) []CompletionItem {
	list, _ := v.([]any)
	var out []CompletionItem
	for _, item := range list {
		switch it := item.(type) {
		case string:
			out = append(out, CompletionItem{Text: sanitize.Line(it)})
		case map[string]any:
			text, _ := it["text"].(string)
			desc, _ := it["desc"].(string)
			out = append(out, CompletionItem{Text: sanitize.Line(text), Desc: sanitize.Line(desc)})
		}
	}
	return slices.DeleteFunc(out, func(c CompletionItem) bool { return c.Text == "" })
}

// WorkflowMentions lists saved workflows for the @ popup's Workflows
// section: name and description, a mention attaching how to run one.
func (h *Host) WorkflowMentions() []CompletionItem {
	var out []CompletionItem
	for _, w := range h.workflows() {
		out = append(out, CompletionItem{Text: w.Name, Desc: listDesc(w.Description)})
	}
	return out
}

// AgentMentions lists agent definitions for the @ popup's Agents section:
// name and description, a mention attaching how to start one.
func (h *Host) AgentMentions() []CompletionItem {
	var out []CompletionItem
	for _, d := range h.agentDefs() {
		out = append(out, CompletionItem{Text: d.Name, Desc: listDesc(d.Description)})
	}
	return out
}

// DidYouMean returns the name nearest got among names, "" when none is
// within an edit distance of 2 (the CLI root's near-miss distance).
func DidYouMean(got string, names []string) string {
	got, best, dist := strings.ToLower(got), "", 3
	for _, n := range names {
		if d := tools.EditDistance(got, strings.ToLower(n)); d < dist {
			best, dist = n, d
		}
	}
	return best
}

// addCompletion is zemi.composer.add(spec).
func (h *Host) addCompletion(L *glua.LState) int {
	spec := L.CheckTable(1)
	fn, _ := opt[*glua.LFunction](L, spec, "items")
	s := completionSource{
		id: rawString(L, spec, "id"), trigger: rawString(L, spec, "trigger"), command: rawString(L, spec, "command"),
		fn: fn, origin: h.RT.CurrentOrigin(L), path: h.RT.CurrentPath(L), gen: h.currentGen(),
	}
	r, size := utf8.DecodeRuneInString(s.trigger)
	bad := size != len(s.trigger) || !(unicode.IsPunct(r) || unicode.IsSymbol(r)) || r == '/'
	switch {
	case s.id == "" || fn == nil:
		L.RaiseError("zemi.composer.add: id and items are required")
	case (s.trigger == "") == (s.command == ""):
		L.RaiseError("zemi.composer.add: set trigger or command, not both")
	case s.trigger != "" && bad:
		L.RaiseError("zemi.composer.add: trigger must be one punctuation character other than /, got %q", s.trigger)
	}
	reg := &h.completions
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if i := slices.IndexFunc(reg.list, func(o completionSource) bool { return o.id == s.id }); i >= 0 {
		if err := mayReplace(h.PluginOf, "completion source "+s.id, reg.list[i].origin, reg.list[i].path, s.origin, s.path); err != nil {
			L.RaiseError("zemi.composer.add: %v", err)
		}
		reg.list[i] = s
		return 0
	}
	reg.list = append(reg.list, s)
	return 0
}
