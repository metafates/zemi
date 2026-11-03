// export.go is what a chat can be copied out of: /export writes the
// conversation as markdown, /copy puts a reply or a code block on the
// clipboard, and /usage reports what it cost. It also caches the todo list the
// chat widget shows.

package hostapi

import (
	"cmp"
	"encoding/json/v2"
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
)

// CopyReply copies the viewed run's, else the live session's, last reply as
// the model wrote it (a drag copies rendered, wrapped screen rows). args is
// "" (the whole reply at once, or the picker over its code blocks when it
// has any, so one can be picked), "reply", "code" (every block) or
// "code <n>".
func (h *Host) CopyReply(args string) error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	f := strings.Fields(args)
	text, err := h.lastReply()
	if err != nil {
		return err
	}
	text = sanitize.String(text)
	if text == "" {
		h.Notify(slog.LevelWarn, "Nothing to copy yet", "")
		return nil
	}
	blocks := codeBlocks(text)
	switch {
	case len(f) == 0 && len(blocks) == 0, len(f) == 1 && f[0] == "reply":
		ui.Copy(text)
	case len(f) == 0:
		ui.OpenOverlay(copyPicker(text, blocks))
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

// copyPicker is the picker bare /copy opens over a reply with code blocks:
// each block numbered, then the direct forms as rows. A pick copies itself
// and the picker stays open, so the next block is one move away.
func copyPicker(text string, blocks []codeBlock) Overlay {
	o := Overlay{ID: "copy", Title: "Copy", Origin: lua.OriginBuiltin, Anchor: OverlayBottomRight, CloseOnCopy: true}
	for i, b := range blocks {
		head := ""
		for l := range strings.Lines(b.text) {
			if first := strings.TrimSpace(l); first != "" {
				head = clipLine(first, 60)
				break
			}
		}
		desc := pluralCount(lineCount(strings.TrimRight(b.text, "\n")), "line", "lines")
		if head != "" {
			desc += "  " + head
		}
		o.Items = append(o.Items, OverlayItem{
			Text: fmt.Sprintf("%d  %s", i+1, cmp.Or(b.lang, "text")),
			Desc: desc, Copy: b.text,
		})
	}
	parts := make([]string, len(blocks))
	for i, b := range blocks {
		parts[i] = strings.TrimSuffix(b.text, "\n")
	}
	o.Items = append(
		o.Items,
		OverlayItem{Text: "every block", Desc: pluralCount(len(blocks), "code block", "code blocks"), Copy: strings.Join(parts, "\n\n") + "\n"},
		OverlayItem{Text: "the whole reply", Desc: pluralCount(lineCount(text), "line", "lines"), Copy: text},
	)
	return o
}

// lastReply is the reply /copy and its direct forms take: the viewed run's
// streamed one included, else the log's newest agent row.
func (h *Host) lastReply() (string, error) {
	if run := h.viewedRun(); run != nil {
		return lastReplyText(run.transcript()), nil
	}
	if h.DB == nil {
		return "", nil
	}
	rows, err := h.DB.LogKindBack(store.KindAgent, 1)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[len(rows)-1].Text, nil
}

// lastReplyText is the last non-empty assistant reply of transcript rows,
// reasoning and tool rows passed over.
func lastReplyText(tr []ChatMessage) string {
	for _, m := range slices.Backward(tr) {
		if m.Role == fantasy.MessageRoleAssistant && m.Kind == RowMessage {
			if s := strings.TrimSpace(m.Text); s != "" {
				return s
			}
		}
	}
	return ""
}

// lineCount counts a text's lines, a closing newline not starting another.
func lineCount(s string) int { return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1 }

// realLines counts the lines an output stands for: the marker a cut leaves
// names what it folded away.
func realLines(s string) int {
	n := 0
	for l := range strings.SplitSeq(strings.TrimSuffix(s, "\n"), "\n") {
		if m := readMoreLines(l); m > 0 {
			n += m
			continue
		}
		n++
	}
	return n
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

// ExportSession writes the chat log as markdown to path ("" picks
// zemi-<seq>.md in the project), says where, and returns the path written.
func (h *Host) ExportSession(path string, full bool) (string, error) {
	if h.DB == nil {
		return "", Usagef("nothing to export")
	}
	if path == "" {
		path = filepath.Join(h.Dirs.ProjectDir, "zemi-chat.md")
	} else if !filepath.IsAbs(path) {
		path = filepath.Join(h.Dirs.ProjectDir, path)
	}
	// the user names this path, so it may leave the project
	md, err := LogMarkdown(h.DB, full)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		return "", err
	}
	// the bare name says nothing of where: a file under the project says so,
	// anything else its whole tilde path
	note := "saved to " + perm.TildePath(path)
	if rel, err := filepath.Rel(h.Dirs.ProjectDir, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		note = "saved to " + rel + " in the project"
	}
	h.Notify(slog.LevelInfo, "Chat exported", note)
	return path, nil
}

// LogMarkdown is the chat log as markdown; full expands every tool call with
// its result.
func LogMarkdown(db *store.DB, full bool) (string, error) {
	rows, err := db.LogFrom(0, exportMaxRows)
	if err != nil {
		return "", err
	}
	count, err := db.LogCount()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# zemi chat\n\n_%d messages, %s_\n", count, time.Now().Format(StampLayout))
	if count > int64(len(rows)) {
		fmt.Fprintf(&b, "\n_(the first %d messages are older than this export)_\n", count-int64(len(rows)))
	}
	exportLog(&b, rows, 2, full)
	return b.String(), nil
}

// exportMaxRows bounds one export; the log is eternal, a markdown file is
// not the archive.
const exportMaxRows = 5000

// exportLog writes log rows as markdown under level headers, one over each
// run of rows by one speaker. Every string is sanitized: a file the user
// later cats must not carry escapes.
func exportLog(b *strings.Builder, rows []store.LogEntry, level int, full bool) {
	speaker := ""
	for _, row := range rows {
		who := "zemi"
		if row.Kind == store.KindUser || row.Kind == store.KindShell {
			who = "You"
		}
		if who != speaker || who == "You" {
			fmt.Fprintf(b, "\n%s %s\n", strings.Repeat("#", level), who)
			speaker = who
		}
		text := strings.TrimSpace(sanitize.String(row.Text))
		name := rowTool(row)
		switch row.Kind {
		case store.KindTool:
			if full {
				fmt.Fprintf(b, "\n**%s**\n", sanitize.String(name))
				b.WriteString(codeFence("", text))
			} else {
				head := sanitize.Line(toolSummary(name, row.Text))
				fmt.Fprintf(b, "- `%s` %s\n", sanitize.String(name), clipLine(head, 120))
			}
		case store.KindEcho:
			if full {
				b.WriteString(codeFence("", text))
			}
		case store.KindWork:
			fmt.Fprintf(b, "\n_%s_\n", text)
		case store.KindNote:
			fmt.Fprintf(b, "\n> _note: %s_\n", text)
		case store.KindThought:
			// reasoning joins the export in full only, as the command says
			if full {
				fmt.Fprintf(b, "\n> _thought:_\n")
				for line := range strings.Lines(text) {
					fmt.Fprintf(b, "> %s", line)
				}
			}
		case store.KindError:
			fmt.Fprintf(b, "\n> _error: %s_\n", strings.ReplaceAll(text, "\n", " "))
		case store.KindShell:
			var meta struct {
				OK  bool   `json:"ok"`
				Out string `json:"out"`
			}
			_ = json.Unmarshal([]byte(row.MetaJSON), &meta)
			if full {
				fmt.Fprintf(b, "\n**!command**\n")
				b.WriteString(codeFence("bash", text))
				if strings.TrimSpace(meta.Out) != "" {
					b.WriteString(codeFence("", meta.Out))
				}
			} else {
				fmt.Fprintf(b, "- `!%s`\n", clipLine(firstLine(text), 120))
			}
		default:
			fmt.Fprintf(b, "\n%s\n", demoteHeadings(text, level))
		}
	}
}

// demoteHeadings sinks each ATX heading outside a code fence by level, to six
// hashes at most (seven are no heading), so a message's own "## Plan" stays
// under the speaker header it is written in.
// NOTE: a setext heading (a line underlined with === or ---) is not demoted.
func demoteHeadings(s string, level int) string {
	var b strings.Builder
	var fs fenceScanner
	for line := range strings.SplitAfterSeq(s, "\n") {
		body := strings.TrimLeft(line, " ")
		wasFence := fs.run != ""
		switch {
		case fs.line(line, 0):
		case !wasFence && len(line)-len(body) < 4 && atxHeading.MatchString(body):
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
		if run, _ := fenceRun(strings.TrimLeft(l, " \t")); strings.HasPrefix(run, "`") {
			n = max(n, len(run)+1)
		}
	}
	fence := strings.Repeat("`", n)
	return fence + lang + "\n" + text + "\n" + fence + "\n"
}

// todoSnapshot is the live todo list for widgets, reloaded only after the
// todos tool wrote.
func (h *Host) todoSnapshot() []store.Todo {
	sid := h.AgentSessionID()
	h.todoMu.Lock()
	defer h.todoMu.Unlock()
	if h.todoSID != sid || h.todosStale.Swap(false) {
		h.todoSID, h.todos = sid, nil
		if h.DB != nil {
			h.todos, _ = h.DB.Todos(sid)
		}
	}
	return h.todos
}

// fmtTokens reads a token count as the statusline does: 1234 is 1.2k, 1500000000 is 1.5B.
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e9), ".0") + "B"
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	case n >= 1000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e3), ".0") + "k"
	default:
		return strconv.FormatInt(n, 10)
	}
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
// all time of this project's chat, dated by the day each bill landed.
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
	section := func(title string, note ...Segment) {
		if len(rows) > 0 {
			rows = append(rows, []Segment{})
		}
		rows = append(rows, append([]Segment{text(ChromeTitle, title)}, note...))
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
		pct := contextPct(s.LastIn, s.CtxWindow)
		hl := ChromeHintKey
		if pct >= 95 {
			hl = ChromeDanger
		} else if pct >= 80 {
			hl = ChromeWarn
		}
		pctText := strconv.Itoa(int(pct)) + "%"
		if pct < 1 {
			pctText = fmt.Sprintf("%.1f%%", pct) // a filled cell beside a rounded "0%" reads as a lie
		}
		section("Context")
		rows = append(rows, append(append([]Segment{text("", "  ")}, bar(s.LastIn, s.CtxWindow, 32, hl)...),
			text(ChromeValue, fmtTokens(s.LastIn)), text(ChromeHint, fmt.Sprintf(" of %s  %s", fmtTokens(s.CtxWindow), pctText))))
	}
	if parts, total := h.promptBreakdown(); total > 0 {
		section("Prompt", text(ChromeValue, "  "+fmtTokens(total)), text(ChromeHint, " sent with every request"))
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
	// the view's own size: what the eternal chat keeps the next turn reading
	if b, lines, levels, unbuilt := h.viewStats(); lines > 0 {
		section("View", text(ChromeHint, " the chat the model reads"))
		detail := fmt.Sprintf("  %s  %s  %s", fmtBytes(int64(b)), pluralCount(lines, "line", "lines"), pluralCount(levels, "level", "levels"))
		if unbuilt > 0 {
			detail += "  " + pluralCount(unbuilt, "line", "lines") + " unbuilt"
		}
		rows = append(rows, []Segment{text(ChromeValue, detail)})
	}
	type window struct {
		label string
		u     store.Usage
	}
	windows := []window{{"this session", store.Usage{Prompt: s.SessionIn, Completion: s.SessionOut, Cost: s.SessionCost}}}
	y, m, d := time.Now().UTC().Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
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
	// the live session's cache share, when there is one: a broken prefix has
	// no other symptom than the bill. It takes its own row under the column
	// head: on the head's row it wraps off its label on a narrow screen, and
	// only the live session answers it - a column of blanks would too
	cached := ""
	if s.CacheBilled > 0 {
		cached = fmt.Sprintf("cached %.1f%%", float64(s.CacheRead)*100/float64(s.CacheBilled))
	}
	head := fmt.Sprintf("%18s%9s", "in", "out") // over the rows' columns, past the 6 cells of the title
	if priced {
		head += "      cost"
	}
	section("Tokens", text(ChromeHint, head))
	if cached != "" {
		rows = append(rows, []Segment{text(ChromeHint, "  "+cached)})
	}
	for _, w := range windows {
		row := []Segment{text(ChromeValue, fmt.Sprintf("  %-14s%8s%9s", w.label, fmtTokens(w.u.Prompt), fmtTokens(w.u.Completion)))}
		if priced {
			row = append(row, text(ChromeValue, fmt.Sprintf("%10s", fmtCost(w.u.Cost))))
		}
		rows = append(rows, row)
	}
	if id != "" && !priced {
		rows = append(rows, []Segment{}, []Segment{text(ChromeHint, "No cost: "+id+" has no prices set.")})
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
