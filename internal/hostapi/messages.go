// messages.go is the messages picker: the conversation as one list, a row
// per message, with the chat scrolling to whatever the selection names. It
// is how an earlier prompt or reply is found and copied.

package hostapi

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
)

// OpenMessages opens the picker over what the chat shows: the session, or
// the agent run it was told to show.
func (h *Host) OpenMessages() error {
	ui, err := h.Screen()
	if err != nil {
		return err
	}
	ui.OpenOverlay(messagesPicker(h.viewedTranscript()))
	return nil
}

// viewedTranscript is the chat's rows: the session's, or the shown run's,
// read the way a render reads them.
func (h *Host) viewedTranscript() []ChatMessage {
	sess, stream := h.liveView()
	if run := h.viewedRun(); run != nil {
		_, rows, _, _ := run.view(stream)
		return rows
	}
	return sess.Transcript
}

// messagesPicker lists the rows under a section per turn: what the user
// typed, the replies, notices, errors and commands between, a reply's code
// blocks hanging under it and the runs a click can open. Thinking and other
// calls stay out: they are the activity a turn folds into one digest, not
// messages.
func messagesPicker(tr []ChatMessage) Overlay {
	o := Overlay{
		ID: "messages", Title: "Messages", Origin: lua.OriginBuiltin,
		Anchor: OverlayBottomRight, Follow: true, Empty: "No messages yet.",
	}
	section, turn := "", 0
	var texts [][]Segment
	add := func(item map[string]any, segs []Segment) {
		item["section"] = section
		o.Items = append(o.Items, item)
		texts = append(texts, segs)
	}
	for i, m := range tr {
		n := i + 1 // the chat's own row number, as its anchors carry it
		text := strings.TrimSpace(m.Text)
		switch {
		case m.Kind == RowMessage && m.Role == fantasy.MessageRoleUser:
			if text == "" {
				continue
			}
			turn++
			section = "Turn " + strconv.Itoa(turn)
			var desc []string
			if m.SentAt > 0 {
				desc = append(desc, time.UnixMilli(m.SentAt).Format(ClockLayout))
			}
			if c := lineCount(text); c > 1 {
				desc = append(desc, pluralCount(c, "line", "lines"))
			}
			add(messageItem(n, text, strings.Join(desc, " · "), "▌", "UserMsg"))
		case m.Kind == RowMessage:
			if text == "" {
				continue
			}
			blocks := codeBlocks(text)
			var desc []string
			if c := lineCount(text); c > 1 {
				desc = append(desc, pluralCount(c, "line", "lines"))
			}
			if len(blocks) > 0 {
				desc = append(desc, pluralCount(len(blocks), "code block", "code blocks"))
			}
			add(messageItem(n, text, strings.Join(desc, " · "), "◆", "Dim"))
			for _, b := range blocks {
				lang := cmp.Or(b.lang, "text")
				item, segs := messageItem(n, "⎿ "+lang+" · "+pluralCount(lineCount(b.text), "line", "lines"), firstLine(b.text), "", "")
				item["copy"] = b.text
				add(item, segs)
			}
		case m.Kind == RowNotice, m.Kind == RowCompact:
			if text == "" {
				continue
			}
			item, segs := messageItem(n, text, "", "", "")
			item["mark"] = "note"
			add(item, segs)
		case m.Kind == RowError:
			item, segs := messageItem(n, text, "", "", "")
			switch m.Status {
			case CallCancelled:
				item["mark"] = "stop"
			default:
				item["mark"] = "err"
			}
			add(item, segs)
		case m.Kind == RowShell:
			item, segs := messageItem(n, m.Text, "", "", "")
			item["text"] = "$ " + firstLine(m.Text)
			add(item, segs)
		case m.Kind == RowTool && m.Run != "":
			view, what := "agents view ", "agent run"
			if m.Tool == "workflow" {
				view, what = "workflows view ", "workflow run"
			}
			item, segs := messageItem(n, cmp.Or(m.Label, text), what, "", "")
			item["run"] = view + m.Run
			add(item, segs)
		}
	}
	o.ItemText = texts
	return o
}

// messageItem is a list row for transcript row n: its head line as the
// label, desc after it and its whole text as what y copies. A glyph and its
// theme group lead the label as the speaker's kind mark, and the row's
// segments come back for the overlay's ItemText.
func messageItem(n int, text, desc, glyph, hl string) (map[string]any, []Segment) {
	head := clipRunes(messageHead(text), 60)
	item := map[string]any{"anchor": n, "text": head}
	if body := strings.TrimSpace(text); body != "" {
		item["copy"] = body
	}
	if desc != "" {
		item["desc"] = desc
	}
	if glyph == "" {
		return item, nil
	}
	return item, []Segment{{Text: glyph + " ", HL: hl}, {Text: head}}
}

// messageHead is the line a message reads by: its first non-blank one, a
// fence line passed over so a reply that opens with a block reads as its
// code; the first non-blank line when it is all fences.
func messageHead(text string) string {
	head := ""
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			head = cmp.Or(head, line)
			continue
		}
		return line
	}
	return head
}

// pluralCount is "1 line", "2 code blocks".
func pluralCount(n int, one, many string) string {
	return fmt.Sprintf("%d %s", n, Plural(n, one, many))
}

// firstLine is text's first line, trimmed.
func firstLine(text string) string {
	head, _, _ := strings.Cut(text, "\n")
	return strings.TrimSpace(head)
}
