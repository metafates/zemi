package ui

import (
	"context"
	"log/slog"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/metafates/zemi/internal/hostapi"
)

// cmdPalette is the composer's slash-command popup (DESIGN §6.6): enter runs
// the typed line, with the highlighted command in place of a typed name that
// names none or that the user moved off; an exact "/reload", the Go-reserved
// force reload, never reaches the registry (DESIGN §4).
type cmdPalette struct {
	open     bool
	matches  []*hostapi.Command
	hits     []hostapi.Hit // what the query matched in each of matches
	args     []listItem    // argument candidates once the cursor is past "/name "
	argStart int           // rune index where the argument under the cursor starts
	selected int
	moved    bool   // the user moved the highlight in this list
	query    string // what the list was narrowed by; a new one selects its first row
	view     viewport
	size     boxSize // the last drawn size, kept for a frame with no matches to show
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
		cmds := a.host.Commands.List()
		if string(r) == "/reload" { // enter reloads, so no row may claim it
			cmds = nil
		}
		a.palette.rematch(string(r[1:nameEnd]), strings.TrimSpace(string(r[nameEnd:])) != "", cmds)
		return
	}
	// Past "/name ": complete the argument under the cursor.
	start := cur
	for start > nameEnd && r[start-1] != ' ' {
		start--
	}
	name, typed, line := string(r[1:nameEnd]), string(r[start:cur]), string(r[:cur])
	// cands come ranked by CompleteCommand or as a completion source returned
	// them, so only narrow drops rows
	show := func(cands []listItem, narrow bool) {
		p := &a.palette
		if p.args == nil || p.argStart != start { // a new list: the name's gave way, or another argument's
			p.selected, p.moved, p.view = 0, false, viewport{}
		} else if p.query != typed {
			p.selected, p.moved = 0, false
		}
		p.query, p.matches, p.hits, p.argStart = typed, nil, nil, start
		p.args = matchRows(cands, typed, narrow)
		p.open = len(p.args) > 0
		p.selected = max(0, min(p.selected, len(p.args)-1))
	}
	lua := a.host.HasCompletions("", name)
	if c := a.host.Commands.Lookup(name); !lua && (c == nil || c.Complete == nil || c.GoComplete != nil) {
		show(candRows(a.host.CompleteCommand(context.Background(), name, typed, line), ""), false)
		return
	}
	// A Lua completer or source runs on the worker, since the key loop never
	// waits on Lua; its answer is dropped once the line before the cursor
	// moved on. Until it lands, the rows it gave narrow by the typed word.
	show(a.palette.args, true)
	a.background(func() {
		cands := a.host.CompleteCommand(context.Background(), name, typed, line)
		if lua {
			cands = append(cands, a.host.Complete(context.Background(), "", name, typed, line)...)
		}
		a.do(func() {
			if string(a.composer.runes()[:a.composer.cursor]) == line {
				show(candRows(cands, ""), false)
			}
		})
	})
}

func (p *cmdPalette) close() {
	p.open = false
	p.selected, p.moved, p.view, p.size, p.query = 0, false, viewport{}, boxSize{}, ""
	p.matches, p.args, p.hits = nil, nil, nil
}

// rematch ranks the command names the typed one matches, best first; a bare
// "/" lists every command in order. With arguments after the name only the
// names it begins are listed, since a fuzzy hit would rewrite a line the
// user may have meant as typed.
func (p *cmdPalette) rematch(query string, args bool, cmds []*hostapi.Command) {
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Name
	}
	hits := hostapi.Match(query, names, nil)
	if query != "" {
		hits = hostapi.Rank(hits, names, len(hits))
	}
	p.matches, p.hits = p.matches[:0], p.hits[:0]
	for _, h := range hits {
		// hidden commands stay out of the list until their name is typed
		if (!cmds[h.Index].Hidden || h.Prefix) && (!args || h.Prefix || query == "") {
			p.matches, p.hits = append(p.matches, cmds[h.Index]), append(p.hits, h)
		}
	}
	if p.query != query {
		p.selected, p.moved = 0, false
	}
	p.query = query
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
		a.composer.replace(p.argStart, p.args[p.selected].label+" ")
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
		return emptyRows(noMatches, maxW)
	}
	items := make([]listItem, 0, p.count())
	for i, c := range p.matches {
		var match []int
		for _, at := range p.hits[i].Pos {
			match = append(match, at+1) // past the "/"
		}
		items = append(items, listItem{label: "/" + c.Name, desc: c.Desc, match: match})
	}
	items = append(items, p.args...)
	return listRowsW(items, items, p.selected, &p.view, min(8, room), minW, maxW)
}

// dismissPalette closes the palette and its line with it: the line was the
// palette's query, and esc on it means abandon - the doc's "esc clears it".
// ctrl+z still walks the cleared draft back.
func (a *App) dismissPalette() {
	a.clearComposer()
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
// agent (DESIGN §6.2, §6.3). A draft the cap refuses (a recalled message, a
// paste token's text) refuses whole, the draft kept for trimming.
func (a *App) submitComposer() {
	text := strings.TrimSpace(a.composer.expand(a.composer.text))
	if text == "" {
		return
	}
	if utf8.RuneCountInString(text) > maxDraftChars {
		a.draftCapToastNow()
		return
	}
	if strings.HasPrefix(text, "/") {
		a.runSlashCommand(text) // a command leaves the reader where they are
		return
	}
	a.followStage()
	a.sendMessage(text)
}

// runSlashCommand executes a "/name args" line. Raw "/reload" (exact, case
// sensitive, non-abbreviable) is the Go-reserved force-reload trigger and
// never reaches the Lua-visible command registry (DESIGN §4, §6.2).
func (a *App) runSlashCommand(text string) {
	raw := strings.TrimPrefix(text, "/")
	if raw == "reload" {
		a.clearComposer()
		// off the UI goroutine: replaying the boot may ask for trust
		a.host.GoSafe("reload", a.host.Reload)
		return
	}
	name, args := hostapi.SplitCommand(raw)
	cmd := a.host.Commands.Lookup(name)
	if cmd == nil {
		detail := "/" + name + "; /help lists them"
		var names []string
		for _, c := range a.host.Commands.List() {
			names = append(names, c.Name)
		}
		if near := hostapi.DidYouMean(name, names); near != "" {
			detail += "; did you mean /" + near + "?"
		}
		// the line goes with the palette: a kept half-command only glues the
		// toast's suggested retype onto itself; the toast is the record
		a.pushToast(slog.LevelWarn, "Unknown command", detail)
		a.clearComposer()
		return
	}
	a.clearComposer()
	a.runCommand(cmd.Name, args, false)
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
		a.applyScroll(hostapi.ScrollBottom, regionNone)
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
	a.palette.close()
	a.completion.close()
	return len(q)
}
