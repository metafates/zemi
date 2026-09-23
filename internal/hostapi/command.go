package hostapi

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	glua "github.com/yuin/gopher-lua"
)

// BuiltinCommandsStep registers the Go-owned slash commands: the ones the
// domain files define plus extra, which main.go declares because they need
// the program's own hooks. Builtin origin lets user config replace them.
func (h *Host) BuiltinCommandsStep(extra ...*Command) lua.BootStep {
	return lua.BootStep{
		Name: "builtin commands",
		Run: func(ctx context.Context, r *lua.Runtime, g *lua.Generation) error {
			cmds := slices.Concat(extra, h.memoryCommands(), h.reflectorCommands(), h.skillsCommands(),
				h.agentsCommands(), h.workflowCommands(), h.mcpCommands(), h.lspCommands())
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

// validNargs follows Vim's spec strings (§6.2).
func validNargs(n string) bool {
	switch n {
	case "", "?", "+", "*":
		return true
	}
	return false
}

// nargsOK validates a raw argument string against command name's nargs spec.
// Attached text (piped stdin) is no argument.
func nargsOK(name, nargs, args string) error {
	typed, _ := agent.CutAttached(args)
	fields := strings.Fields(typed)
	switch nargs {
	case "":
		if len(fields) != 0 {
			return fmt.Errorf("/%s takes no arguments", name)
		}
	case "?":
		if len(fields) > 1 {
			return fmt.Errorf("/%s takes at most one argument", name)
		}
	case "+":
		if len(fields) == 0 {
			return fmt.Errorf("/%s requires an argument", name)
		}
	}
	return nil
}

func (h *Host) commandNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.command.add")
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
			cmd.Desc, cmd.NArgs = rawString(opts, "desc"), rawString(opts, "nargs")
			if !validNargs(cmd.NArgs) {
				L.RaiseError("zemi.command.add: invalid nargs %q (want one of \"\", \"?\", \"+\", \"*\")", cmd.NArgs)
			}
			cmd.Complete, _ = opts.RawGetString("complete").(*glua.LFunction)
		}
		if _, err := h.Commands.Add(cmd); err != nil {
			L.RaiseError("zemi.command.add: %v", err)
		}
		return 0
	}))

	// del(name) removes a command registered by this origin or a
	// lower-trust one; it returns whether one was removed.
	tb.RawSetString("del", L.NewFunction(func(L *glua.LState) int {
		h.refuseDuringRender(L, "zemi.command.del")
		ok, err := h.Commands.Remove(L.CheckString(1), h.RT.CurrentOrigin(L))
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
		h.refuseDuringRender(L, "zemi.command.run")
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
					h.Notify("error", "/"+name+" failed", err.Error())
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
// command's Go completer or its Lua complete(arg_lead, cmdline), sanitized
// and capped; a failing completer offers nothing and lands in /errors.
func (h *Host) CompleteCommand(ctx context.Context, name, lead, line string) []CompletionItem {
	cmd := h.Commands.Lookup(name)
	if cmd == nil {
		return nil
	}
	var out []CompletionItem
	switch {
	case cmd.GoComplete != nil:
		for _, s := range cmd.GoComplete(h, lead, line) {
			out = append(out, CompletionItem{Text: sanitize.String(s)})
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
		out = completionItems(res[0])
	}
	return out[:min(len(out), maxCompletions)]
}

// subcommandCompleter completes the first argument from a fixed list and,
// when next is set, later arguments from next.
func subcommandCompleter(subs []string, next func(h *Host, sub string) []string) func(h *Host, lead, line string) []string {
	return func(h *Host, lead, line string) []string {
		fields := strings.Fields(line)
		arg := len(fields)
		if lead != "" {
			arg--
		}
		switch {
		case arg <= 1:
			return subs
		case next != nil:
			return next(h, fields[1])
		}
		return nil
	}
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

func (h *Host) runLuaCommand(ctx context.Context, cmd *Command, argString string) error {
	_, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		if h.currentGen() != cmd.Gen {
			return nil, lua.ErrReloaded
		}
		defer h.RT.PushFrame(L, cmd.Origin, cmd.Path)()
		base := L.GetTop()
		L.Push(cmd.Fn)
		L.Push(commandArgs(L, argString))
		if err := L.PCall(1, 0, nil); err != nil {
			L.SetTop(base)
			return nil, err
		}
		return nil, nil
	})
	return err
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

// RunRHS executes a keymap right-hand side on the ui class, attributed to
// the binding's registration origin (DESIGN §9 attribution rule). Host
// actions run on the caller's goroutine (never the interpreter), so config
// watches they fire go back through the queue.
func (h *Host) RunRHS(ctx context.Context, b *KeyBinding) error {
	switch rhs := b.RHS; {
	case rhs.Fn != nil:
		_, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			// looked up before a reload, the function belongs to a dropped LState
			if h.currentGen() != b.Gen {
				return nil, lua.ErrReloaded
			}
			defer h.RT.PushFrame(L, b.Origin, "")()
			base := L.GetTop()
			L.Push(rhs.Fn)
			if err := L.PCall(0, 0, nil); err != nil {
				L.SetTop(base)
				return nil, err
			}
			return nil, nil
		})
		return err
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
