package hostapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
)

// BuiltinCommandsStep registers the Go-owned slash commands: the ones the
// domain files define plus extra, which main.go declares because they need
// the program's own hooks. Builtin origin lets user config replace them.
func (h *Host) BuiltinCommandsStep(extra ...*Command) lua.BootStep {
	return lua.BootStep{
		Name: "builtin commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			cmds := slices.Concat(extra, h.skillsCommands(),
				h.agentsCommands(), h.jobsCommands(), h.callCommands(),
				h.memoriesCommands(), h.workflowCommands(), h.mcpCommands(),
				h.lspCommands(), h.pluginsCommands(), h.configCommands(),
				h.projectsCommands(), h.keysCommands(), h.chatCommands())
			for _, c := range cmds {
				c.Origin, c.Gen = lua.OriginBuiltin, Gen(g.ID)
				if _, err := h.Commands.Add(c); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// NArgs is how many arguments a command takes, in Vim's spec strings
// (§6.2).
type NArgs string

const (
	NArgsNone     NArgs = ""
	NArgsOptional NArgs = "?"
	NArgsSome     NArgs = "+"
	NArgsAny      NArgs = "*"
)

var _nargsSpecs = []NArgs{NArgsNone, NArgsOptional, NArgsSome, NArgsAny}

// nargsOK validates a raw argument string against command name's nargs spec.
// Attached text is no argument. A failed arity is a usage error, the kind a
// warn toast answers - a driver's typo is not a recorded failure.
func nargsOK(name string, nargs NArgs, args string) error {
	typed, _ := agent.CutAttached(args)
	fields := strings.Fields(typed)
	switch nargs {
	case NArgsNone:
		if len(fields) != 0 {
			return Usagef("/%s takes no arguments", name)
		}
	case NArgsOptional:
		if len(fields) > 1 {
			return Usagef("/%s takes at most one argument", name)
		}
	case NArgsSome:
		if len(fields) == 0 {
			return Usagef("/%s requires an argument", name)
		}
	case NArgsAny:
	}
	return nil
}

func (h *Host) commandNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		fn := L.CheckFunction(2)
		cmd := &Command{
			Name:   name,
			Fn:     fn,
			Origin: h.RT.CurrentOrigin(L),
			Path:   h.RT.CurrentPath(L),
			Gen:    h.currentGen(),
		}
		if opts := L.OptTable(3, nil); opts != nil {
			cmd.Desc, cmd.NArgs = rawString(L, opts, "desc"), NArgs(rawString(L, opts, "nargs"))
			if !slices.Contains(_nargsSpecs, cmd.NArgs) {
				L.RaiseError("zemi.command.add: invalid nargs %q (want one of %q)", cmd.NArgs, _nargsSpecs)
			}
			cmd.Complete, _ = opt[*glua.LFunction](L, opts, "complete")
		}
		if _, err := h.Commands.Add(cmd); err != nil {
			L.RaiseError("zemi.command.add: %v", err)
		}
		return 0
	}))

	// del(name) removes a command the caller may replace (mayReplace); it
	// returns whether one was removed.
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		ok, err := h.Commands.Remove(L.CheckString(1), h.RT.CurrentOrigin(L), h.RT.CurrentPath(L))
		if err != nil {
			L.RaiseError("zemi.command.del: %v", err)
		}
		L.Push(glua.LBool(ok))
		return 1
	}))

	// run(name [, args]) runs a command as if typed. A Lua command runs
	// right here under its own origin; a host command runs off the
	// interpreter (it may start a turn), so run returns before it finishes.
	tb.RawSetString("run", L.NewFunction(func(L *glua.LState) int {
		name := L.CheckString(1)
		parts := make([]string, 0, max(L.GetTop()-1, 0))
		for i := 2; i <= L.GetTop(); i++ {
			parts = append(parts, L.CheckString(i))
		}
		args := strings.Join(parts, " ")
		cmd := h.Commands.Lookup(name)
		if cmd == nil {
			return luaResult(L, fmt.Errorf("not a command: %s", name))
		}
		if err := nargsOK(name, cmd.NArgs, args); err != nil {
			return luaResult(L, err)
		}
		if cmd.GoFn != nil {
			h.GoSafe("command "+name, func() {
				if err := cmd.GoFn(h, args, false); err != nil {
					h.CommandFailed(name, err)
				}
			})
			return luaResult(L, nil)
		}
		_, err := h.callFn(L, luaCaller{gen: cmd.Gen, origin: cmd.Origin, path: cmd.Path}, cmd.Fn, 0, lua.FromGo(L, commandArgs(args)))
		return luaResult(L, err)
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		var rows []any
		for _, c := range h.Commands.List() {
			rows = append(rows, map[string]any{"name": c.Name, "desc": c.Desc, "nargs": c.NArgs, "origin": string(c.Origin)})
		}
		L.Push(lua.FromGo(L, rows))
		return 1
	}))

	return tb
}

// CompleteCommand returns argument candidates for a command line from the
// command's Go completer or its Lua complete(arg_lead, cmdline), sanitized,
// matched against lead, ranked and capped; a failing completer offers
// nothing and lands in /errors, and nothing is offered past the last
// argument the command's nargs takes.
func (h *Host) CompleteCommand(ctx context.Context, name, lead, line string) []CompletionItem {
	cmd := h.Commands.Lookup(name)
	if _, arg := argAt(lead, line); cmd == nil || cmd.NArgs == NArgsNone || cmd.NArgs == NArgsOptional && arg > 1 {
		return nil
	}
	var cands []CompletionItem
	switch {
	case cmd.GoComplete != nil:
		for _, s := range cmd.GoComplete(h, lead, line) {
			cands = append(cands, CompletionItem{Text: sanitize.String(s)})
		}
	case cmd.Complete != nil:
		res, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, "command completer", func(L *glua.LState) ([]any, error) {
			// looked up before a reload, the function belongs to a dropped LState
			if h.currentGen() != cmd.Gen {
				return nil, lua.ErrReloaded
			}
			items, err := h.callCompleter(L, cmd.Complete, luaCaller{gen: cmd.Gen, origin: cmd.Origin, path: cmd.Path}, lead, line)
			return []any{items}, err
		})
		if err != nil {
			if !lua.Dropped(err) {
				h.Log.Error("command completer failed", "origin", "ui", "command", name, "error", err)
				h.RecordError("command", name+" complete", err.Error())
			}
			return nil
		}
		cands, _ = res[0].([]CompletionItem)
	}
	// lead narrows and ranks here, before the cap: the one hit may sit past it
	labels := make([]string, len(cands))
	for i, c := range cands {
		labels[i] = c.Text
	}
	hits := Match(lead, labels, nil)
	if lead != "" {
		hits = Rank(hits, labels, _maxCompletions)
	}
	var out []CompletionItem
	for _, hit := range hits[:min(len(hits), _maxCompletions)] {
		out = append(out, cands[hit.Index])
	}
	return out
}

// SubcommandCompleter completes the first argument from a fixed list and,
// when next is set, the second from next: what follows is a value or text.
func SubcommandCompleter(subs []string, next func(h *Host, sub string) []string) func(h *Host, lead, line string) []string {
	return func(h *Host, lead, line string) []string {
		fields, arg := argAt(lead, line)
		switch {
		case arg <= 1:
			return subs
		case arg == 2 && next != nil:
			return next(h, fields[1])
		default:
			return nil
		}
	}
}

// argAt splits the line before the cursor into words, "/name" first, and
// says which argument, from 1, the cursor is in.
func argAt(lead, line string) ([]string, int) {
	fields := strings.Fields(line)
	if lead != "" {
		return fields, len(fields) - 1
	}
	return fields, len(fields)
}

// _usageToastBytes is the body length past which a usage refusal cannot fit
// the toast's three rows on a common terminal: cut there, its payload would
// name no place to read on. A wide terminal could have shown more; the body
// is readable in /errors either way.
const _usageToastBytes = 100

// CommandFailed reports a command's error: one of how it was asked
// (ErrUsage) is a warn, anything else did not happen and is recorded. A
// usage body too long for the toast rides /errors, where its hint points.
func (h *Host) CommandFailed(name string, err error) {
	if errors.Is(err, ErrUsage) && len(err.Error()) <= _usageToastBytes {
		// a usage warn is the driver's input, not a failure: the command
		// names itself and the body says what it needed
		h.Notify(slog.LevelWarn, "/"+name, err.Error())
		return
	}
	h.RecordError("command", name, err.Error())
	h.Notify(slog.LevelError, "/"+name, err.Error())
}

// SendPrompt sends a prompt as the composer would: one naming a registered
// command runs it with its attachment, so /review, /workflows run and the
// markdown commands reach a script, a git hook or a loop; anything else is
// a message for the model.
func (h *Host) SendPrompt(prompt string) error {
	_, _, err := h.sendPrompt(prompt)
	return err
}

// sendPrompt is SendPrompt, reporting whether the message joined a running
// turn, and whether a command ran instead of a message starting a turn.
func (h *Host) sendPrompt(prompt string) (queued, ran bool, err error) {
	typed, attached := agent.CutAttached(prompt)
	if rest, ok := strings.CutPrefix(typed, "/"); ok {
		if name, args := SplitCommand(rest); h.Commands.Lookup(name) != nil {
			err = h.RunCommand(context.Background(), name, args+attached, false)
			if err != nil && !errors.Is(err, ErrNeedsTUI) {
				// the sender reads the error; the window's user sees it too.
				// A needs-the-TUI error is the surface's to name, not a failure
				h.CommandFailed(name, err)
			}
			return false, true, err
		}
	}
	queued, err = h.agentSendUser(prompt)
	return queued, false, err
}

// ConfirmArgs splits a command line's consent: confirm is an armed danger
// key's press and answers by itself, else a typed trailing " --yes" does and
// comes off the rest.
func ConfirmArgs(args string, confirm bool) (rest string, yes bool) {
	rest, yes = strings.CutSuffix(strings.TrimSpace(args), " --yes")
	return rest, yes || confirm
}

// RunCommand runs a command by name as if typed. confirm says the caller
// already consented (a danger key's armed press), so a command that asks
// first may act; the typed line passes false and carries its own --yes.
func (h *Host) RunCommand(ctx context.Context, name, argString string, confirm bool) error {
	cmd := h.Commands.Lookup(name)
	if cmd == nil {
		return fmt.Errorf("not a command: %s", name)
	}
	if err := nargsOK(name, cmd.NArgs, argString); err != nil {
		return err
	}
	if cmd.GoFn != nil {
		err := cmd.GoFn(h, argString, confirm)
		if errors.Is(err, ErrNeedsTUI) {
			err = fmt.Errorf("/%s %w", name, err)
		}
		return err
	}
	return h.runLuaCommand(ctx, cmd, argString)
}

// sendCommandPrompt speaks a command's line to the model as if the user had
// typed it: the /name line with its typed arguments, then the elements
// riding behind it (the command's prompt, the user's attachments), in
// order. The /cmd prefix is model-facing decoration: AgentSend does not
// route a line to a command (only SendPrompt does), so it names what ran
// for the model; it runs nothing.
func (h *Host) sendCommandPrompt(name, typed string, riding ...string) error {
	return h.AgentSend("/" + name + " " + typed + strings.Join(riding, ""))
}

// AskFirst opens a destructive command's screen with the action whose run is
// line armed (Overlay.Confirm); without a screen it refuses, naming shell, or
// /line, as what runs it without asking.
func AskFirst(line, shell string, open func(confirm string) error) error {
	err := open(line)
	if errors.Is(err, ErrNeedsTUI) {
		err = fmt.Errorf("%w to ask first; %s runs it without asking", err, cmp.Or(shell, "/"+line))
	}
	return err
}

// runLuaCommand runs a Lua command as a task; it returns once the task
// suspends, so the worker runs the next key before its rest.
func (h *Host) runLuaCommand(ctx context.Context, cmd *Command, argString string) error {
	t := h.luaTask(lua.ClassUI, luaCaller{gen: cmd.Gen, origin: cmd.Origin, path: cmd.Path}, cmd.Fn, commandArgs(argString))
	return h.RT.StartTask(ctx, t, func(err error) {
		h.refresh()
		if err != nil && !lua.Dropped(err) {
			h.CommandFailed(cmd.Name, err)
		}
	})
}

// commandArgs is a command handler's argument: { args = raw, list = words }.
func commandArgs(argString string) map[string]any {
	typed, _ := agent.CutAttached(argString)
	return map[string]any{"args": argString, "list": strings.Fields(typed)}
}

// RunRHS executes a keymap right-hand side on the ui class, a function as a
// task, attributed to the binding's registration origin (DESIGN §9
// attribution rule), and reports its error, a later slice's included. Host
// actions run on the caller's goroutine (never the interpreter), so config
// watches they fire go back through the queue.
func (h *Host) RunRHS(ctx context.Context, b *KeyBinding) (err error) {
	report := func(err error) {
		switch {
		case err == nil:
		case lua.Dropped(err):
			h.Log.Debug("keymap dropped", "origin", "ui", "keys", b.Key, "error", err)
		default:
			h.RecordError("keymap", b.Key, err.Error())
			h.Log.Error("keymap rhs failed", "origin", "ui", "keys", b.Key, "error", err)
			h.Notify(slog.LevelError, "Could not run "+b.Key, err.Error())
		}
	}
	defer func() { report(err) }()
	switch rhs := b.RHS; {
	case rhs.Fn != nil:
		return h.RT.StartTask(ctx, h.luaTask(lua.ClassUI, luaCaller{gen: b.Gen, origin: b.Origin, path: b.Path}, rhs.Fn), func(err error) {
			h.refresh()
			report(err)
		})
	case rhs.Command != "":
		return h.RunCommand(ctx, rhs.Command, "", false)
	case rhs.API != "":
		name, args, _ := strings.Cut(rhs.API, " ")
		fn, ok := h.hostActions[name]
		if !ok || fn == nil {
			return fmt.Errorf("%w: %q", errReserved, name)
		}
		return fn(h, args)
	default:
		return nil
	}
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
