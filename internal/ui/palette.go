package ui

import (
	"context"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// cmdPalette is the composer's slash-command popup (DESIGN §6.6): typing
// "/" as the first character lists the registered commands filtered as you
// type; up/down navigate, tab accepts, esc closes. Enter runs the typed
// "/name args" line instead of sending a message, or the highlighted
// command when the typed name is not one. The exact "/reload" is
// the Go-reserved force-reload trigger and never reaches the command
// registry (DESIGN §4).
type cmdPalette struct {
	open     bool
	matches  []*hostapi.Command
	args     []hostapi.CompletionItem // argument candidates once the cursor is past "/name "
	argStart int                      // rune index where the argument under the cursor starts
	selected int
	moved    bool   // the user moved the highlight in this argument list
	query    string // what the list was narrowed by; a new one selects its first row
	view     viewport
	size     boxSize // typing never narrows or shortens the popup
}

func (p *cmdPalette) count() int {
	if p.args != nil {
		return len(p.args)
	}
	return len(p.matches)
}

// updatePalette recomputes the palette after a composer edit: it opens
// while the cursor sits in the first word and that word starts with "/".
func (a *App) updatePalette() {
	r := a.composer.runes()
	cur := a.composer.cursor
	if cur == 0 || len(r) == 0 || r[0] != '/' {
		a.palette.close()
		return
	}
	nameEnd := 1
	for nameEnd < len(r) && r[nameEnd] != ' ' && r[nameEnd] != '\n' {
		nameEnd++
	}
	if cur <= nameEnd {
		a.palette.open, a.palette.args = true, nil
		a.palette.rematch(string(r[1:nameEnd]), a.host.Commands.List())
		return
	}
	// Past "/name ": complete the argument under the cursor.
	start := cur
	for start > nameEnd && r[start-1] != ' ' {
		start--
	}
	name, typed, line := string(r[1:nameEnd]), string(r[start:cur]), string(r[:cur])
	show := func(cands []hostapi.CompletionItem) {
		if p := &a.palette; p.args == nil || p.argStart != start { // a new list: the name's gave way, or another argument's
			p.selected, p.moved, p.view, p.size = 0, false, viewport{}, boxSize{}
		} else if p.query != typed {
			p.selected, p.moved = 0, false
		}
		a.palette.query = typed
		lead := strings.ToLower(typed)
		args := []hostapi.CompletionItem{}
		for _, c := range cands {
			if strings.HasPrefix(strings.ToLower(c.Text), lead) {
				args = append(args, c)
			}
		}
		a.palette.matches, a.palette.args, a.palette.argStart = nil, args, start
		a.palette.open = len(args) > 0
		a.palette.selected = max(0, min(a.palette.selected, len(args)-1))
	}
	lua := a.host.HasCompletions("", name)
	if c := a.host.Commands.Lookup(name); !lua && (c == nil || c.Complete == nil || c.GoComplete != nil) {
		show(a.host.CompleteCommand(context.Background(), name, typed, line))
		return
	}
	// A Lua completer or source runs on the worker, since the key loop never
	// waits on Lua; its answer is dropped once the line before the cursor
	// moved on. Until it lands, the rows it gave narrow by prefix.
	show(a.palette.args)
	a.background(func() {
		cands := a.host.CompleteCommand(context.Background(), name, typed, line)
		if lua {
			cands = append(cands, a.host.Complete(context.Background(), "", name, typed, line)...)
		}
		a.do(func() {
			if string(a.composer.runes()[:a.composer.cursor]) == line {
				show(cands)
			}
		})
	})
}

func (p *cmdPalette) close() {
	p.open = false
	p.selected, p.moved, p.view, p.size, p.query = 0, false, viewport{}, boxSize{}, ""
	p.matches, p.args = nil, nil
}

// rematch filters the command registry against the typed prefix. The
// prefix is the first word with or without the trigger character; an
// empty prefix (bare "/") lists every command.
func (p *cmdPalette) rematch(prefix string, cmds []*hostapi.Command) {
	p.matches = p.matches[:0]
	prefix = strings.ToLower(strings.TrimPrefix(prefix, "/"))
	if i := strings.IndexAny(prefix, " \t"); i >= 0 {
		prefix = prefix[:i]
	}
	for _, c := range cmds {
		// hidden commands stay out of the bare "/" list but complete once typed
		if strings.HasPrefix(strings.ToLower(c.Name), prefix) && (!c.Hidden || prefix != "") {
			p.matches = append(p.matches, c)
		}
	}
	if p.query != prefix {
		p.selected = 0
	}
	p.query = prefix
	p.selected = max(0, min(p.selected, len(p.matches)-1))
}

// acceptPalette fills the composer with the selected "/name " or argument,
// then offers what comes next, as a typed space would.
func (a *App) acceptPalette() tea.Cmd {
	p := &a.palette
	if !p.open || p.selected >= p.count() || p.selected < 0 {
		return nil
	}
	if p.args != nil {
		a.composer.replace(p.argStart, p.args[p.selected].Text+" ")
	} else {
		a.composer.text = "/" + p.matches[p.selected].Name + " "
		a.composer.moveEnd()
	}
	p.close()
	return a.updateCompletion()
}

// paletteRows renders the popup's list, at most room rows of at most maxW
// cells, padded to minW.
func (a *App) paletteRows(maxW, room, minW int) [][]hostapi.Segment {
	p := &a.palette
	if p.count() == 0 {
		return emptyRows("No matches.", maxW)
	}
	items := make([]listItem, 0, p.count())
	for _, c := range p.matches {
		items = append(items, listItem{label: "/" + c.Name, desc: c.Desc})
	}
	for _, c := range p.args {
		items = append(items, listItem{label: c.Text, desc: c.Desc})
	}
	return listRowsW(items, items, p.selected, &p.view, min(8, room), minW, maxW)
}

// clearComposer empties the draft and closes both popups. The paste map
// stays: the undo stack still holds the tokens of the draft being cleared,
// and without their text a ctrl+z sends the placeholder to the model.
func (a *App) clearComposer() {
	a.composer.text, a.composer.cursor = "", 0
	a.palette.close()
	a.completion.close()
}

// submitComposer routes the composer submit: slash lines run commands,
// everything else goes through the vetoable composer.submit event and the
// agent (DESIGN §6.2, §6.3).
func (a *App) submitComposer() {
	text := strings.TrimSpace(a.composer.expand(a.composer.text))
	if text == "" {
		return
	}
	if strings.HasPrefix(text, "/") {
		a.runSlashCommand(text) // a command leaves the reader where they are
		return
	}
	a.followStage()
	// !command runs in the user's shell; the output joins the next message
	if cmd, ok := strings.CutPrefix(text, "!"); ok && strings.TrimSpace(cmd) != "" {
		a.composer.pushHistory(text, a.historySize())
		a.clearComposer()
		a.host.RunShell(cmd)
		return
	}
	a.sendMessage(text)
}

// runSlashCommand executes a "/name args" line. Raw "/reload" (exact, case
// sensitive, non-abbreviable) is the Go-reserved force-reload trigger and
// never reaches the Lua-visible command registry (DESIGN §4, §6.2).
func (a *App) runSlashCommand(text string) {
	raw := strings.TrimPrefix(text, "/")
	if raw == "reload" {
		a.clearComposer()
		if a.reload != nil {
			// off the UI goroutine: replaying project config may ask for trust
			go a.reload()
		}
		return
	}
	name, args := SplitCommand(raw)
	cmd := a.host.Commands.Lookup(name)
	if cmd == nil {
		a.pushToast(logging.Warn, "Unknown command", "/"+name+"; /help lists them")
		return
	}
	a.clearComposer()
	a.runCommand(cmd.Name, args)
}

// sendMessage runs the composer.submit vetoable event, then appends the
// message to the transcript and drives the agent.
func (a *App) sendMessage(text string) {
	res := a.host.EmitVetoable(context.Background(), "composer.submit", map[string]any{"text": text}, lua.VetoDeadline)
	if res.Denied {
		a.pushToast(logging.Warn, "Message blocked", res.Reason)
		return
	}
	draft := a.composer.text
	a.composer.pushHistory(text, a.historySize())
	a.clearComposer()
	if id := a.host.ViewedAgent(); id != "" {
		if a.host.MessageAgent(id, text) == nil {
			return
		}
		_ = a.host.ViewAgent("") // the run is over: messages go to the session, so show it
	}
	if err := a.host.AgentSend(text); err != nil {
		a.pushToast(logging.Error, "Could not send", err.Error())
		a.composer.text = draft
		a.composer.cursor = len(a.composer.runes())
		a.publishComposer()
	}
}

// restoreQueue moves queued steering messages back into the composer,
// ahead of any draft, so they can be edited and sent again.
func (a *App) restoreQueue() bool {
	q := a.host.AgentTakeQueue()
	if len(q) == 0 {
		return false
	}
	if !a.composer.empty() {
		q = append(q, a.composer.text)
	}
	a.composer.text = strings.Join(q, "\n\n")
	a.composer.moveEnd()
	return true
}

// SplitCommand splits "name arg string" into the command name and the raw
// argument string. The name ends at any whitespace: a shift+enter line may
// follow it.
func SplitCommand(raw string) (name, args string) {
	i := strings.IndexFunc(raw, unicode.IsSpace)
	if i < 0 {
		return raw, ""
	}
	return raw[:i], strings.TrimLeftFunc(raw[i:], unicode.IsSpace)
}
