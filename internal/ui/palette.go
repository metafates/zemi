package ui

import (
	"strings"

	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/lua"
)

// cmdPalette is the composer's slash-command popup (DESIGN §6.6): typing
// "/" as the first character lists the registered commands filtered as you
// type; up/down navigate, tab/enter accept, esc closes. Submitting a
// "/name args" line runs the command instead of sending a message. The
// exact, case-sensitive "/Reload" is the Go-reserved force-reload trigger
// and never reaches the command registry (DESIGN §4).
type cmdPalette struct {
	open     bool
	matches  []*hostapi.Command
	args     []string // argument candidates once the cursor is past "/name "
	argStart int      // rune index where the argument under the cursor starts
	selected int
}

func (p *cmdPalette) count() int {
	if p.args != nil {
		return len(p.args)
	}
	return len(p.matches)
}

// paletteTrigger reads the configured trigger character; a config value
// that is not exactly one rune falls back to "/".
func (a *App) paletteTrigger() string {
	if v, ok := a.host.Settings.Get("ui.palette.trigger"); ok {
		if s, ok := v.(string); ok && len([]rune(s)) == 1 {
			return s
		}
	}
	return "/"
}

// updatePalette recomputes the palette after a composer edit: it opens
// while the cursor sits in the first word and that word starts with the
// trigger character.
func (a *App) updatePalette() {
	if !a.paletteEnabled() {
		a.palette.close()
		return
	}
	tr := a.paletteTrigger()
	r := a.composer.runes()
	cur := a.composer.cursor
	if cur == 0 || len(r) == 0 || r[0] != rune(tr[0]) {
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
	lead := strings.ToLower(string(r[start:cur]))
	args := []string{}
	for _, c := range a.host.CompleteCommand(a.ctx(), string(r[1:nameEnd]), string(r[start:cur]), string(r[:cur])) {
		if strings.HasPrefix(strings.ToLower(c), lead) {
			args = append(args, c)
		}
	}
	a.palette.matches, a.palette.args, a.palette.argStart = nil, args, start
	a.palette.open = len(args) > 0
	a.palette.selected = max(0, min(a.palette.selected, len(args)-1))
}

func (a *App) paletteEnabled() bool { return a.cfgBool("ui.palette.enabled", true) }

func (p *cmdPalette) close() {
	p.open = false
	p.selected = 0
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
		if strings.HasPrefix(strings.ToLower(c.Name), prefix) {
			p.matches = append(p.matches, c)
		}
	}
	p.selected = max(0, min(p.selected, len(p.matches)-1))
}

func (p *cmdPalette) move(d int) { p.selected = max(0, min(p.selected+d, p.count()-1)) }

// accept fills the composer with the selected "/name " and closes.
func (a *App) acceptPalette() {
	p := &a.palette
	if !p.open || p.selected >= p.count() || p.selected < 0 {
		return
	}
	if p.args != nil {
		r := a.composer.runes()
		repl := p.args[p.selected] + " "
		a.composer.text = string(r[:p.argStart]) + repl + string(r[a.composer.cursor:])
		a.composer.cursor = p.argStart + len([]rune(repl))
		p.close()
		return
	}
	a.composer.text = "/" + p.matches[p.selected].Name + " "
	a.composer.cursor = len([]rune(a.composer.text))
	a.composer.moveEnd()
	p.close()
}

// paletteRows renders the popup rows shown above the composer.
func (a *App) paletteRows(w int) [][]hostapi.Segment {
	p := &a.palette
	if !p.open || p.count() == 0 {
		return nil
	}
	items := make([]listItem, 0, p.count())
	for _, c := range p.matches {
		items = append(items, listItem{"/" + c.Name, c.Desc})
	}
	for _, s := range p.args {
		items = append(items, listItem{label: s})
	}
	return listRows(items, p.selected, a.cfgInt("ui.palette.max_items", 8, 1, 20), "Title")
}

// submitComposer routes the composer submit: slash lines run commands,
// everything else goes through the vetoable composer.submit event and the
// agent (DESIGN §6.2, §6.3).
func (a *App) submitComposer() {
	text := strings.TrimSpace(a.composer.text)
	if text == "" {
		return
	}
	a.chatFollow = true
	if a.paletteEnabled() && strings.HasPrefix(text, a.paletteTrigger()) {
		a.runSlashCommand(text)
		return
	}
	// !command runs in the user's shell; the output joins the next message
	if cmd, ok := strings.CutPrefix(text, "!"); ok && strings.TrimSpace(cmd) != "" {
		a.composer.pushHistory(text, a.historySize())
		a.composer.text, a.composer.cursor = "", 0
		a.completion.close()
		a.host.RunShell(cmd)
		return
	}
	a.sendMessage(text)
}

// runSlashCommand executes a "/name args" line. Raw "/Reload" (exact, case
// sensitive, non-abbreviable) is the Go-reserved force-reload trigger and
// never reaches the Lua-visible command registry (DESIGN §4, §6.2).
func (a *App) runSlashCommand(text string) {
	raw := strings.TrimPrefix(text, a.paletteTrigger())
	if raw == "Reload" {
		a.composer.text, a.composer.cursor = "", 0
		a.palette.close()
		if a.reload != nil {
			a.reload()
		}
		return
	}
	name, args := splitCommand(raw)
	cmd := a.host.Commands.Lookup(name)
	if cmd == nil {
		a.pushToast("warn", "command", "not a command: /"+name)
		return
	}
	a.composer.text, a.composer.cursor = "", 0
	a.palette.close()
	a.completion.close()
	if err := a.host.RunCommand(a.ctx(), cmd.Name, args); err != nil {
		a.pushToast("error", "/"+name, err.Error())
	}
}

// sendMessage runs the composer.submit vetoable event, then appends the
// message to the transcript and drives the agent (M1 data flow).
func (a *App) sendMessage(text string) {
	res := a.host.EmitVetoable(a.ctx(), "composer.submit", map[string]any{"text": text}, lua.VetoDeadline)
	if res.Denied {
		a.pushToast("warn", "composer", "blocked: "+res.Reason)
		return
	}
	a.composer.pushHistory(text, a.historySize())
	a.composer.text = ""
	a.composer.cursor = 0
	a.completion.close()
	a.palette.close()
	if err := a.host.AgentSend(text); err != nil {
		a.pushToast("error", "agent", err.Error())
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

// splitCommand splits "name arg string" into the command name and the raw
// argument string.
func splitCommand(raw string) (name, args string) {
	if i := strings.Index(raw, " "); i >= 0 {
		return raw[:i], strings.TrimLeft(raw[i:], " ")
	}
	return raw, ""
}
