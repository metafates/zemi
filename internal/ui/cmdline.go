package ui

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// cmdlineState is the ":" command line (DESIGN §6.6). The Go-reserved
// :Reload dispatches here before any Lua-visible command lookup.
type cmdlineState struct {
	open   bool
	prefix string
	text   string
	err    string
	cands  []string // tab-completion candidates for the current prefix
}

func (a *App) openCmdline(prefix string) {
	if prefix == "" {
		prefix = ":"
	}
	a.cmdline = cmdlineState{open: true, prefix: prefix}
}

func (a *App) closeCmdline() {
	a.cmdline = cmdlineState{}
}

// cmdlineKey handles keys while the cmdline is open; returns a cmd when a
// command ran (the caller may need to arm follow-ups).
func (a *App) cmdlineKey(key string, msg tea.KeyPressMsg) tea.Cmd {
	switch key {
	case "esc":
		a.closeCmdline()
	case "enter":
		return a.executeCmdline()
	case "tab":
		a.completeCmdline()
	case "backspace":
		a.cmdline.err = ""
		a.cmdline.cands = nil
		if r := []rune(a.cmdline.text); len(r) > 0 {
			a.cmdline.text = string(r[:len(r)-1])
		}
	default:
		if t := keyText(msg); t != "" {
			a.cmdline.err = ""
			a.cmdline.cands = nil
			a.cmdline.text += t
		}
	}
	return nil
}

// completeCmdline completes the command name against the registry: one
// candidate fills the line, several fill the common prefix and list the
// rest, and an empty prefix lists everything.
func (a *App) completeCmdline() {
	name, _, hasArgs := strings.Cut(a.cmdline.text, " ")
	if hasArgs {
		return
	}
	var names []string
	for _, c := range a.host.Commands.List() {
		if strings.HasPrefix(c.Name, name) {
			names = append(names, c.Name)
		}
	}
	sort.Strings(names)
	switch {
	case len(names) == 0:
		a.cmdline.cands = nil
	case len(names) == 1:
		a.cmdline.text = names[0] + " "
		a.cmdline.cands = nil
	default:
		lcp := names[0]
		for _, n := range names[1:] {
			for !strings.HasPrefix(n, lcp) {
				lcp = lcp[:len(lcp)-1]
			}
		}
		if len(lcp) > len(name) {
			a.cmdline.text = lcp
		}
		a.cmdline.cands = names
	}
}

// executeCmdline runs the typed command. Raw "Reload" (exact, case
// sensitive, non-abbreviable) is the Go-reserved force-reload trigger and
// never reaches the Lua-visible command registry (DESIGN §4, §6.2).
func (a *App) executeCmdline() tea.Cmd {
	raw := strings.TrimRight(a.cmdline.text, " ")
	if raw == "" {
		a.closeCmdline()
		return nil
	}
	if a.cmdline.prefix == ":" && raw == "Reload" {
		a.closeCmdline()
		if a.reload != nil {
			a.reload()
		}
		return nil
	}
	name, args := splitCommand(raw)
	if err := a.host.RunCommand(a.ctx(), name, args); err != nil {
		a.cmdline.err = err.Error()
		return nil
	}
	a.closeCmdline()
	return nil
}

// splitCommand splits "Name arg string" into the command name and the raw
// argument string.
func splitCommand(raw string) (name, args string) {
	if i := strings.Index(raw, " "); i >= 0 {
		return raw[:i], strings.TrimLeft(raw[i:], " ")
	}
	return raw, ""
}
