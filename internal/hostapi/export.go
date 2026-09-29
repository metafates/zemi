// export.go is what a session can be copied out of: /export writes the
// conversation as markdown, /copy puts a reply or a code block on the
// clipboard, /fork continues it in a new session so the original stays as
// it was, and /usage reports what it cost. It also caches the todo list the
// chat widget shows.

package hostapi

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sanitize"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// CopyReply copies the live session's last reply as the model wrote it (a
// drag copies rendered, wrapped screen rows). args is "" (a picker when the
// reply has code blocks), "reply", "code" (every block) or "code <n>".
func (h *Host) CopyReply(args string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		h.Notify(slog.LevelWarn, "Nothing to copy", "")
		return nil
	}
	rows, err := h.DB.Messages(sid)
	if err != nil {
		return err
	}
	history, _ := historyFrom(rows)
	text := sanitize.String(lastAssistantText(history))
	if text == "" {
		h.Notify(slog.LevelWarn, "Nothing to copy", "")
		return nil
	}
	blocks := codeBlocks(text)
	f := strings.Fields(args)
	switch {
	case len(f) == 0 && len(blocks) == 0, len(f) == 1 && f[0] == "reply":
		ui.Copy(text)
	case len(f) == 0:
		ui.OpenOverlay(copyPicker(blocks))
	case f[0] != "code" || len(f) > 2:
		return Usagef("usage: /copy [reply | code [n]]")
	case len(blocks) == 0:
		return Usagef("the last reply has no code blocks")
	case len(f) == 1:
		parts := make([]string, len(blocks))
		for i, b := range blocks {
			parts[i] = strings.TrimSuffix(b.text, "\n")
		}
		ui.Copy(strings.Join(parts, "\n\n") + "\n")
	default:
		n, err := strconv.Atoi(f[1])
		if err != nil || n < 1 || n > len(blocks) {
			return Usagef("no code block %s: the last reply has %d", f[1], len(blocks))
		}
		ui.Copy(blocks[n-1].text)
	}
	return nil
}

// copyPicker lists a reply's code blocks, then all of them and the whole
// reply; each row runs a /copy form, and 1-9 pick a block at once.
func copyPicker(blocks []codeBlock) Overlay {
	var items []any
	for i, b := range blocks {
		item := map[string]any{"text": fmt.Sprintf("%d  %s", i+1, cmp.Or(b.lang, "text")), "run": fmt.Sprintf("copy code %d", i+1)}
		if i < 9 {
			item["hotkey"] = strconv.Itoa(i + 1)
		}
		n := strings.Count(b.text, "\n")
		if b.text != "" && !strings.HasSuffix(b.text, "\n") {
			n++
		}
		var desc strings.Builder
		fmt.Fprintf(&desc, "%d %s", n, plural(n, "line", "lines"))
		for l := range strings.Lines(b.text) {
			if first := strings.TrimSpace(l); first != "" {
				desc.WriteString(" · " + first)
				break
			}
		}
		item["desc"] = desc.String()
		items = append(items, item)
	}
	if len(blocks) > 1 {
		items = append(items, map[string]any{"text": "All code blocks", "run": "copy code"})
	}
	items = append(items, map[string]any{"text": "Whole reply", "run": "copy reply"})
	return Overlay{ID: "copy", Title: "Copy", Origin: lua.OriginBuiltin, Items: items, Verb: "copy"}
}

type codeBlock struct{ lang, text string }

// codeBlocks returns the fenced code blocks of markdown (``` or ~~~, closed
// by a fence of the same character at least as long) with the fence lines
// cut and the opening fence's indentation taken off each line. Unlike
// CommonMark a fence may be indented any amount, since models nest them in
// list items; an unclosed block runs to the end.
func codeBlocks(md string) []codeBlock {
	var out []codeBlock
	var body strings.Builder
	open, indent := "", 0
	for line := range strings.Lines(md) {
		trimmed := strings.TrimLeft(line, " \t")
		bare := strings.TrimRight(trimmed, " \t\r\n")
		run := "" // the fence characters starting the line
		if strings.HasPrefix(bare, "```") || strings.HasPrefix(bare, "~~~") {
			run = bare[:len(bare)-len(strings.TrimLeft(bare, bare[:1]))]
		}
		switch {
		case open == "" && run != "":
			info := bare[len(run):]
			if run[0] == '`' && strings.Contains(info, "`") {
				continue // inline code such as ```x```
			}
			lang, _, _ := strings.Cut(strings.Join(strings.Fields(info), " "), " ")
			out = append(out, codeBlock{lang: lang})
			open, indent = run, len(line)-len(strings.TrimLeft(line, " "))
			body.Reset()
		case open == "":
		case run == bare && run != "" && run[0] == open[0] && len(run) >= len(open):
			out[len(out)-1].text, open = body.String(), ""
		default:
			cut := min(indent, len(line)-len(strings.TrimLeft(line, " ")))
			body.WriteString(line[cut:])
		}
	}
	if open != "" {
		out[len(out)-1].text = body.String()
	}
	return out
}

// ExportSession writes the live session as markdown to path ("" picks
// zemi-<id>.md in the project), says where, and returns the path written.
func (h *Host) ExportSession(path string, full bool) (string, error) {
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		return "", Usagef("no session to export")
	}
	if path == "" {
		path = filepath.Join(h.Dirs.ProjectDir, "zemi-"+sid[:min(8, len(sid))]+".md")
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(h.Dirs.ProjectDir, path)
	}
	// the user names this path, so it may leave the project; a secret-shaped
	// target is still refused, and only user or builtin code reaches here
	// (zemi.command.run gates Go commands by origin)
	if h.secretPath(path) {
		return "", Usagef("refusing to write over %s", path)
	}
	md, err := SessionMarkdown(h.DB, sid, full)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		return "", err
	}
	h.Notify(slog.LevelInfo, "Session exported", "saved to "+perm.TildePath(h.relPath(path)))
	return path, nil
}

// SessionMarkdown is a session as markdown; full adds reasoning, every tool
// call expanded and the subagent runs.
func SessionMarkdown(db *store.DB, sid string, full bool) (string, error) {
	sess, err := db.GetSession(sid)
	if err != nil {
		return "", err
	}
	rows, err := db.Messages(sid)
	if err != nil {
		return "", err
	}
	title := sanitize.String(sess.Title)
	if title == "" {
		title = "zemi session"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n_zemi session %s, %s_\n", title, sid, time.UnixMilli(sess.CreatedAt).Format(StampLayout))
	_, tr := historyFrom(rows)
	exportMarkdown(&b, tr, 2, full)
	if full {
		// NOTE: raw tool results stay out, the rows' short output stands in;
		// a reader who needs a whole result reads the store
		runs, err := db.ChildSessions(sid)
		if err != nil {
			return "", err
		}
		for _, run := range slices.Backward(runs) {
			rows, err := db.Messages(run.ID)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "\n## Agent run %s (%s): %s\n", run.ID[:min(runIDLen, len(run.ID))], sanitize.String(run.Profile), sanitize.String(run.Title))
			_, tr := historyFrom(rows)
			exportMarkdown(&b, tr, 3, true)
		}
	}
	return b.String(), nil
}

// exportMarkdown writes transcript rows as markdown under level headers, one
// over each run of rows by one speaker; full adds reasoning and expands every
// tool call. Every string is sanitized: a file the user later cats must not
// carry escapes, and historyFrom leaves message text raw.
func exportMarkdown(b *strings.Builder, tr []ChatMessage, level int, full bool) {
	speaker, list := "", false // list: the last line written is a tool row's
	for _, m := range tr {
		if m.Kind == RowReasoning && !full {
			continue
		}
		who := "zemi"
		if m.Role == fantasy.MessageRoleUser {
			who = "You"
		}
		if who != speaker || who == "You" {
			fmt.Fprintf(b, "\n%s %s\n", strings.Repeat("#", level), who)
			speaker, list = who, false
		}
		text := strings.TrimSpace(sanitize.String(m.Text))
		switch {
		case m.Kind == RowReasoning:
			head := "Thought"
			if m.Stat != nil && m.Stat.ThoughtMS > 0 {
				head += " for " + fmtDuration(time.Duration(m.Stat.ThoughtMS)*time.Millisecond)
			}
			fmt.Fprintf(b, "\n> _%s_\n>\n", head)
			for l := range strings.Lines(text) {
				b.WriteString(strings.TrimRight("> "+l, " \n") + "\n")
			}
		case m.Kind == RowTool && full:
			head := strings.Join(strings.Fields(sanitize.String(m.Label)), " ")
			if head == "" && !strings.Contains(text, "\n") {
				head, text = text, ""
			}
			fmt.Fprintf(b, "\n**%s** %s%s\n", sanitize.String(m.Tool), head, callEnd(m.Status))
			lang := ""
			if m.Tool == tools.ToolBash {
				lang = "sh"
			}
			for _, f := range [][2]string{{lang, text}, {"diff", m.Diff}, {"", m.Output}} {
				if f[1] = strings.TrimRight(sanitize.String(f[1]), "\n"); f[1] != "" {
					b.WriteString(codeFence(f[0], f[1]))
				}
			}
			if m.Run != "" {
				fmt.Fprintf(b, "run %s, below\n", sanitize.String(m.Run))
			}
		case m.Kind == RowTool:
			if !list {
				b.WriteString("\n")
			}
			fmt.Fprintf(b, "- `%s` %s%s\n", sanitize.String(m.Tool), sanitize.String(clipRunes(strings.Join(strings.Fields(cmp.Or(m.Label, m.Text)), " "), 120)), callEnd(m.Status))
		case m.Kind == RowNotice, m.Kind == RowCompact:
			fmt.Fprintf(b, "\n_%s_\n", text)
		default:
			fmt.Fprintf(b, "\n%s\n", demoteHeadings(text, level))
		}
		list = m.Kind == RowTool && !full
	}
}

// demoteHeadings sinks each ATX heading outside a code fence by level, to six
// hashes at most (seven are no heading), so a message's own "## Plan" stays
// under the speaker header it is written in.
// NOTE: a setext heading (a line underlined with === or ---) is not demoted.
func demoteHeadings(s string, level int) string {
	var b strings.Builder
	fence := ""
	for line := range strings.SplitAfterSeq(s, "\n") {
		body := strings.TrimLeft(line, " ")
		f := fenceRun(body)
		switch {
		case fence == "" && f != "":
			fence = f
		case fence != "" && strings.HasPrefix(f, fence):
			fence = ""
		case fence == "" && len(line)-len(body) < 4 && atxHeading.MatchString(body):
			n := len(body) - len(strings.TrimLeft(body, "#"))
			line = strings.Repeat("#", min(n+level, 6)) + body[n:]
		}
		b.WriteString(line)
	}
	return b.String()
}

var atxHeading = regexp.MustCompile(`^#{1,6}(\s|$)`)

// codeFence fences text in more backticks than any fence line inside it, so
// a result holding ``` cannot close the block early.
func codeFence(lang, text string) string {
	n := 3
	for l := range strings.Lines(text) {
		if run := fenceRun(strings.TrimLeft(l, " \t")); strings.HasPrefix(run, "`") {
			n = max(n, len(run)+1)
		}
	}
	fence := strings.Repeat("`", n)
	return fence + lang + "\n" + text + "\n" + fence + "\n"
}

// callEnd names a tool call that did not finish well, after its row.
func callEnd(s CallStatus) string {
	switch s {
	case CallErr:
		return " (failed)"
	case CallStopped:
		return " (stopped)"
	default:
		return ""
	}
}

// ForkSession copies the live session's messages into a new session and
// switches to it, saying so; it returns the new id.
func (h *Host) ForkSession() (string, error) {
	sid := h.AgentSessionID()
	if sid == "" || h.DB == nil {
		return "", Usagef("no session to fork")
	}
	if h.AgentActive() {
		return "", errTurnRunning
	}
	if err := h.tasks.stillRunning(func(p string) bool { return p == sid }); err != nil {
		return "", err
	}
	sess, err := h.DB.GetSession(sid)
	if err != nil {
		return "", err
	}
	id := store.NewID()
	if err := h.DB.ForkSession(sess, id, strings.TrimSuffix(sess.Title, " (fork)")+" (fork)"); err != nil {
		return "", err
	}
	// the copied history is the parent's to reflect on when it closes; the
	// fork's delta is only what it adds, so a rejected memory is not proposed twice
	if n, err := h.DB.MessageCount(id); err == nil {
		h.reflectSeen(id, n)
	}
	if err := h.ResumeSession(id); err != nil {
		return "", err
	}
	h.Notify(slog.LevelInfo, "Session forked", "the original stays in /sessions")
	return id, nil
}

// todoSnapshot is the live session's todo list for widgets, reloaded only
// after the todos tool wrote or the session changed.
func (h *Host) todoSnapshot() []store.Todo {
	sid := h.AgentSessionID()
	h.todoMu.Lock()
	defer h.todoMu.Unlock()
	if h.todoSID != sid || h.todosStale.Swap(false) {
		h.todoSID, h.todos = sid, nil
		if sid != "" && h.DB != nil {
			h.todos, _ = h.DB.Todos(sid)
		}
	}
	return h.todos
}

// fmtTokens reads a token count as the statusline does: 1234 is 1.2k.
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	case n >= 1000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e3), ".0") + "k"
	}
	return strconv.FormatInt(n, 10)
}

// fmtCost is a cost in dollars to the cent; one under half a cent is not free.
func fmtCost(usd float64) string {
	if usd > 0 && usd < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", usd)
}

// OpenUsage shows how full the context is, what the fixed prompt is made of,
// and token use and cost: the live session, then today, the last 7 days and
// all time across this project's sessions.
func (h *Host) OpenUsage() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	if h.DB == nil {
		return errors.New("usage needs the session store")
	}
	s := h.AgentStream()
	var rows [][]Segment
	text := func(hl, t string) Segment { return Segment{Text: t, HL: hl} }
	section := func(title, note string) {
		if len(rows) > 0 {
			rows = append(rows, []Segment{})
		}
		rows = append(rows, []Segment{text(ChromeTitle, title), text(ChromeHint, note)})
	}
	// bar is a meter of w cells, part/whole of it filled
	bar := func(part, whole int64, w int, hl string) []Segment {
		n := 0
		if whole > 0 {
			n = int(min(max(part*int64(w)/whole, min(part, 1)), int64(w))) // anything at all gets a cell
		}
		return []Segment{{Text: strings.Repeat("█", n), HL: hl, NoCopy: true}, {Text: strings.Repeat("░", w-n) + "  ", HL: ChromeRule, NoCopy: true}}
	}
	if s.CtxWindow > 0 && s.LastIn > 0 { // a resumed session has no estimate before its first turn
		pct := s.LastIn * 100 / s.CtxWindow
		hl := ChromeHintKey
		if pct >= 90 {
			hl = ChromeDanger
		} else if pct >= 70 {
			hl = ChromeWarn
		}
		section("Context", "")
		rows = append(rows, append(append([]Segment{text("", "  ")}, bar(s.LastIn, s.CtxWindow, 32, hl)...),
			text(ChromeValue, fmtTokens(s.LastIn)), text(ChromeHint, fmt.Sprintf(" of %s · %d%%", fmtTokens(s.CtxWindow), pct))))
	}
	if parts, total := h.promptBreakdown(); total > 0 {
		section("Prompt", "  "+fmtTokens(total)+" sent with every request")
		parts = slices.Clone(parts)
		slices.SortStableFunc(parts, func(a, b promptPart) int { return cmp.Compare(b.chars, a.chars) })
		sum := 0
		for _, p := range parts {
			sum += p.chars
		}
		for _, p := range parts {
			if p.chars == 0 {
				continue
			}
			row := append([]Segment{text(ChromeValue, fmt.Sprintf("  %-14s", p.name))}, bar(int64(p.chars), int64(sum), 16, ChromeHintKey)...)
			rows = append(rows, append(row, text(ChromeValue, fmt.Sprintf("%6s", fmtTokens(int64(p.chars/4)))),
				text(ChromeHint, fmt.Sprintf("  %3d%%", p.chars*100/sum))))
		}
	}
	type window struct {
		label string
		u     store.Usage
	}
	windows := []window{{"this session", store.Usage{Prompt: s.SessionIn, Completion: s.SessionOut, Cost: s.SessionCost}}}
	y, m, d := time.Now().Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	for _, w := range []struct {
		label string
		since time.Time
	}{{"today", midnight}, {"7 days", midnight.AddDate(0, 0, -6)}, {"all time", time.Time{}}} {
		ms := int64(0)
		if !w.since.IsZero() {
			ms = w.since.UnixMilli()
		}
		u, err := h.DB.UsageSince(ms)
		if err != nil {
			return err
		}
		windows = append(windows, window{w.label, u})
	}
	// a model with no prices costs $0.00 on every row: one line says so
	// instead of a column of zeros
	id := h.sessionModelID()
	spec, _ := h.modelSpec(id)
	priced := spec.CostPer1MIn > 0 || spec.CostPer1MOut > 0 || s.SessionCost > 0 || windows[len(windows)-1].u.Cost > 0
	head := fmt.Sprintf("%18s%9s%10s", "in", "out", "sessions") // over the rows' columns, past the 6 cells of the title
	if priced {
		head += "      cost"
	}
	section("Tokens", head)
	for i, w := range windows {
		count := ""
		if w.u.Sessions > 0 {
			count = strconv.Itoa(w.u.Sessions)
		}
		row := []Segment{text(ChromeValue, fmt.Sprintf("  %-14s%8s%9s", w.label, fmtTokens(w.u.Prompt), fmtTokens(w.u.Completion))), text(ChromeHint, fmt.Sprintf("%10s", count))}
		if priced {
			row = append(row, text(ChromeValue, fmt.Sprintf("%10s", fmtCost(w.u.Cost))))
		}
		// a broken cache prefix (a per-turn system section, a model switch) has
		// no other symptom than the bill
		if i == 0 && s.CacheBilled > 0 { // the live session
			row = append(row, text(ChromeHint, fmt.Sprintf("   %d%% cached", s.CacheRead*100/s.CacheBilled)))
		}
		rows = append(rows, row)
	}
	if id != "" && !priced {
		rows = append(rows, []Segment{}, []Segment{text(ChromeHint, "No cost: "+id+" has no prices. Set cost_per_1m_in and cost_per_1m_out on the model.")})
	}
	var plain strings.Builder
	for _, r := range rows {
		for _, seg := range r {
			if !seg.NoCopy {
				plain.WriteString(seg.Text)
			}
		}
		plain.WriteByte('\n')
	}
	ui.OpenOverlay(Overlay{ID: "usage", Title: "Usage", Origin: lua.OriginBuiltin, Body: plain.String(), BodySegs: rows, Anchor: OverlayBottomRight})
	return nil
}
