package hostapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/logging"
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
			cmds := slices.Concat(extra, h.memoryCommands(), h.reflectorCommands(), h.skillsCommands(),
				h.agentsCommands(), h.workflowCommands(), h.mcpCommands(), h.lspCommands(), h.pluginsCommands(), h.configCommands())
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

var nargsSpecs = []NArgs{NArgsNone, NArgsOptional, NArgsSome, NArgsAny}

// nargsOK validates a raw argument string against command name's nargs spec.
// Attached text (piped stdin) is no argument.
func nargsOK(name string, nargs NArgs, args string) error {
	typed, _ := agent.CutAttached(args)
	fields := strings.Fields(typed)
	switch nargs {
	case NArgsNone:
		if len(fields) != 0 {
			return fmt.Errorf("/%s takes no arguments", name)
		}
	case NArgsOptional:
		if len(fields) > 1 {
			return fmt.Errorf("/%s takes at most one argument", name)
		}
	case NArgsSome:
		if len(fields) == 0 {
			return fmt.Errorf("/%s requires an argument", name)
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
			cmd.Desc, cmd.NArgs = rawString(opts, "desc"), NArgs(rawString(opts, "nargs"))
			if !slices.Contains(nargsSpecs, cmd.NArgs) {
				L.RaiseError("zemi.command.add: invalid nargs %q (want one of %q)", cmd.NArgs, nargsSpecs)
			}
			cmd.Complete, _ = opts.RawGetString("complete").(*glua.LFunction)
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
		name, args := L.CheckString(1), L.OptString(2, "")
		cmd := h.Commands.Lookup(name)
		if cmd == nil {
			return luaResult(L, fmt.Errorf("not a command: %s", name))
		}
		if err := nargsOK(name, cmd.NArgs, args); err != nil {
			return luaResult(L, err)
		}
		if cmd.GoFn != nil {
			// a Go-owned command acts with the user's authority (/export
			// writes a file, /reload reloads config), so only the user's own
			// config and zemi's own code may run one: plugin, project and
			// model code go through their own gated APIs instead
			if o := h.RT.CurrentOrigin(L); o != lua.OriginUser && o != lua.OriginBuiltin {
				return luaResult(L, fmt.Errorf("zemi.command.run: %s code may not run /%s", o, name))
			}
			h.GoSafe("command "+name, func() {
				if err := cmd.GoFn(h, args); err != nil {
					h.CommandFailed(name, err)
				}
			})
			return luaResult(L, nil)
		}
		// the body runs under the command's origin, so a lower caller would
		// borrow that authority with arguments it picks
		if o := h.RT.CurrentOrigin(L); cmd.Origin.Rank() > o.Rank() {
			return luaResult(L, fmt.Errorf("zemi.command.run: %s code may not run %s command /%s", o, cmd.Origin, name))
		}
		defer h.RT.PushFrame(L, cmd.Origin, cmd.Path)()
		L.Push(cmd.Fn)
		L.Push(commandArgs(L, args))
		return luaResult(L, L.PCall(1, 0, nil))
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		rows := []any{}
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
		res, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			// looked up before a reload, the function belongs to a dropped LState
			if h.currentGen() != cmd.Gen {
				return nil, lua.ErrReloaded
			}
			defer h.RT.PushFrame(L, cmd.Origin, cmd.Path)()
			base := L.GetTop()
			defer L.SetTop(base)
			L.Push(cmd.Complete)
			L.Push(glua.LString(lead))
			L.Push(glua.LString(line))
			if err := L.PCall(2, 1, nil); err != nil {
				return nil, err
			}
			v, err := lua.ToGo(L.Get(-1))
			return []any{v}, err
		})
		if err != nil {
			if !isQuietLuaErr(err) {
				h.Log.Error("ui", "command completer failed", map[string]any{"command": name, "error": err.Error()})
				h.RecordError("command "+name+" complete", err.Error())
			}
			return nil
		}
		cands = completionItems(res[0])
	}
	// lead narrows and ranks here, before the cap: the one hit may sit past it
	labels := make([]string, len(cands))
	for i, c := range cands {
		labels[i] = c.Text
	}
	hits := Match(lead, labels, nil)
	if lead != "" {
		hits = Rank(hits, labels, maxCompletions)
	}
	var out []CompletionItem
	for _, hit := range hits[:min(len(hits), maxCompletions)] {
		out = append(out, cands[hit.Index])
	}
	return out
}

// subcommandCompleter completes the first argument from a fixed list and,
// when next is set, later arguments from next.
func subcommandCompleter(subs []string, next func(h *Host, sub string) []string) func(h *Host, lead, line string) []string {
	return func(h *Host, lead, line string) []string {
		fields, arg := argAt(lead, line)
		switch {
		case arg <= 1:
			return subs
		case next != nil:
			return next(h, fields[1])
		}
		return nil
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

// CommandFailed reports a command's error: one of how it was asked
// (ErrUsage) is a warn, anything else did not happen and is recorded.
func (h *Host) CommandFailed(name string, err error) {
	if errors.Is(err, ErrUsage) {
		h.Notify(logging.Warn, "Could not run /"+name, err.Error())
		return
	}
	h.RecordError("command "+name, err.Error())
	h.Notify(logging.Error, "Could not run /"+name, err.Error())
}

// RunCommand runs a command by name as if typed.
func (h *Host) RunCommand(ctx context.Context, name, argString string) error {
	cmd := h.Commands.Lookup(name)
	if cmd == nil {
		return fmt.Errorf("not a command: %s", name)
	}
	if err := nargsOK(name, cmd.NArgs, argString); err != nil {
		return err
	}
	if cmd.GoFn != nil {
		err := cmd.GoFn(h, argString)
		if errors.Is(err, ErrNeedsTUI) {
			err = fmt.Errorf("/%s %w", name, err)
		}
		return err
	}
	return h.runLuaCommand(ctx, cmd, argString)
}

// runLuaCommand runs a Lua command as a task. With a UI it returns once the
// task suspends, so the worker runs the next key before its rest; -p waits
// for the whole command.
func (h *Host) runLuaCommand(ctx context.Context, cmd *Command, argString string) error {
	t := lua.Task{
		Class: lua.ClassUI, Origin: cmd.Origin, Path: cmd.Path,
		Start: func(L *glua.LState) (*glua.LFunction, []glua.LValue, error) {
			if h.currentGen() != cmd.Gen {
				return nil, nil, lua.ErrReloaded
			}
			return cmd.Fn, []glua.LValue{commandArgs(L, argString)}, nil
		},
	}
	if h.hooks() == nil {
		_, err := h.RT.RunTask(ctx, t)
		return err
	}
	return h.RT.StartTask(ctx, t, func(err error) {
		h.refresh()
		if err != nil && !isQuietLuaErr(err) {
			h.CommandFailed(cmd.Name, err)
		}
	})
}

// commandArgs is a command handler's argument: { args = raw, list = words }.
func commandArgs(L *glua.LState, argString string) *glua.LTable {
	args := L.NewTable()
	args.RawSetString("args", glua.LString(argString))
	list := L.NewTable()
	typed, _ := agent.CutAttached(argString)
	for i, f := range strings.Fields(typed) {
		list.RawSetInt(i+1, glua.LString(f))
	}
	args.RawSetString("list", list)
	return args
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
		case isQuietLuaErr(err):
			h.Log.Debug("ui", "keymap dropped", map[string]any{"keys": b.Key, "error": err.Error()})
		default:
			h.RecordError("keymap "+b.Key, err.Error())
			h.Log.Error("ui", "keymap rhs failed", map[string]any{"keys": b.Key, "error": err.Error()})
			h.Notify(logging.Error, "Could not run "+b.Key, err.Error())
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
		return h.RunCommand(ctx, rhs.Command, "")
	case rhs.API != "":
		name, args, _ := strings.Cut(rhs.API, " ")
		fn, ok := h.hostActions[name]
		if !ok || fn == nil {
			return ErrReserved
		}
		return fn(h, args)
	}
	return nil
}
