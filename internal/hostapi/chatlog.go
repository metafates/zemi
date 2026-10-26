// chatlog.go is the append-only writer for the main log, the chat that never
// ends. Kinds: user, agent, tool, echo, work, note. Reasoning is never
// logged - no method takes it. Every append commits in one store transaction
// (the log row, its view line and, for the kinds a template can line, the
// message's own node) and then wakes the builder.

package hostapi

import (
	"context"
	"encoding/json/v2"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

// Tool and echo leaves are template-built in the append transaction.
const (
	kindUser  = "user"
	kindAgent = "agent"
	kindTool  = "tool"
	kindEcho  = "echo"
	kindWork  = "work"
	kindNote  = "note"
)

const (
	// chatSplitChars: a longer text becomes several sibling rows of about
	// this many characters rather than being cut.
	chatSplitChars = 8_000
	// echoClipChars: a tool result clips to head and tail, this many
	// characters in all.
	echoClipChars = 30_000
)

// chatLog writes the main log of one project store. It owns the builder.
type chatLog struct {
	h    *Host
	tree *memTree
}

// newChatLog opens the writer and starts the builder worker on ctx: the
// caller's Close ends both.
func newChatLog(ctx context.Context, h *Host) *chatLog {
	return &chatLog{h: h, tree: newMemTree(ctx, h)}
}

// Close ends the builder: its context cancels, in-flight model calls abort,
// and Close returns once every builder goroutine exited.
func (c *chatLog) Close() { c.tree.Close() }

// User logs the user's words.
func (c *chatLog) User(text string) error { return c.append(kindUser, text, "") }

// Agent logs one of zemi's replies.
func (c *chatLog) Agent(text string) error { return c.append(kindAgent, text, "") }

// Note logs a memory from before the chat: a chapter mark.
func (c *chatLog) Note(text string) error { return c.append(kindNote, text, "") }

// Work logs a subagent's report: the run it reports for and the "[Name]" head
// it opens with.
func (c *chatLog) Work(run, name, text string) error {
	meta, err := json.Marshal(struct {
		Run  string `json:"run"`
		Name string `json:"name"`
	}{run, name}, json.Deterministic(true))
	if err != nil {
		return err
	}
	return c.append(kindWork, text, string(meta))
}

// Tool logs one tool call: its name, and the input as sent.
func (c *chatLog) Tool(name, input string) error {
	meta, err := json.Marshal(struct {
		Tool string `json:"tool"`
	}{name}, json.Deterministic(true))
	if err != nil {
		return err
	}
	for _, row := range splitText(input, chatSplitChars) {
		leaf := toolLeaf(name, row)
		if _, err := c.h.DB.AppendLog(kindTool, row, string(meta), "", leaf); err != nil {
			return err
		}
	}
	c.tree.Wake()
	return nil
}

// Echo logs one tool result, clipped to head and tail: the one kind of long
// text that is cut, never split.
func (c *chatLog) Echo(name string, ok bool, output string) error {
	meta, err := json.Marshal(struct {
		Tool string `json:"tool"`
		Ok   bool   `json:"ok"`
	}{name, ok}, json.Deterministic(true))
	if err != nil {
		return err
	}
	clipped := clipMiddle(output, echoClipChars)
	if _, err := c.h.DB.AppendLog(kindEcho, clipped, string(meta), "", echoLeaf(name, ok, clipped)); err != nil {
		return err
	}
	c.tree.Wake()
	return nil
}

// append logs the rows a text splits into, each with its view line.
func (c *chatLog) append(kind, text, meta string) error {
	for _, row := range splitText(text, chatSplitChars) {
		if _, err := c.h.DB.AppendLog(kind, row, meta, "", ""); err != nil {
			return err
		}
	}
	c.tree.Wake()
	return nil
}

// toolLeaf renders a tool call's line mechanically: the tool and the first
// path- or command-shaped value of its input.
// NOTE: sparser than a model line, and the subject is the first plausible
// field, not the right one; merges re-compress these later.
func toolLeaf(name, input string) string {
	line := "tool " + name
	if s := inputSubject(input); s != "" {
		line += " " + s
	}
	return clipBytes(line, nodeBytes)
}

// echoLeaf renders a tool result's line mechanically: the tool, its outcome
// and the head of the output.
// NOTE: sparser than a model line; merges re-compress these later.
func echoLeaf(name string, ok bool, output string) string {
	state := "ok"
	if !ok {
		state = "failed"
	}
	line := "echo " + name + " " + state
	if out := firstText(output, nodeBytes); out != "" {
		line += ": " + out
	}
	return clipBytes(line, nodeBytes)
}

// inputSubject picks the input field that says what the call acted on.
func inputSubject(input string) string {
	var fields map[string]any
	if err := json.Unmarshal([]byte(input), &fields); err != nil {
		return ""
	}
	for _, key := range []string{"path", "file", "dir", "cmd", "command", "url", "pattern", "query"} {
		if s, ok := fields[key].(string); ok && s != "" {
			return s
		}
	}
	keys := slices.Sorted(maps.Keys(fields))
	for _, key := range keys {
		if s, ok := fields[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// splitText cuts a long text into rows of about chars runes each, at a
// newline when one is near the cut: a long text is never cut, it is logged
// as several messages in a row.
func splitText(text string, chars int) []string {
	if utf8.RuneCountInString(text) <= chars {
		return []string{text}
	}
	r := []rune(text)
	out := make([]string, 0, len(r)/chars+1)
	for len(r) > 0 {
		n := min(len(r), chars)
		if n < len(r) { // more follows: cut at a newline near the end when one is there
			lo := max(0, n-200)
			for j, c := range slices.Backward(r[lo:n]) {
				if c == '\n' {
					n = lo + j + 1
					break
				}
			}
		}
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return out
}

// clipMiddle clips s to about chars runes, its head and tail with a marker
// between: a tool result keeps its start and its end.
func clipMiddle(s string, chars int) string {
	r := []rune(s)
	if len(r) <= chars {
		return s
	}
	head := chars / 2
	return string(r[:head]) + "\n...\n" + string(r[len(r)-(chars-head):])
}

// firstText is the text's first line, at most chars runes: the error a
// failed result names sits at its head.
func firstText(s string, chars int) string {
	head, _, _ := strings.Cut(s, "\n")
	return clipBytes(head, chars)
}

// clipBytes cuts s to at most bytes UTF-8 bytes without splitting a rune.
func clipBytes(s string, bytes int) string {
	if len(s) <= bytes {
		return s
	}
	cut := bytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
