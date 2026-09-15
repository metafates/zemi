package hostapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/sanitize"
	glua "github.com/yuin/gopher-lua"
)

// validNargs follows Vim's spec strings (§6.2).
func validNargs(n string) bool {
	switch n {
	case "", "?", "+", "*":
		return true
	}
	return false
}

// nargsOK validates a raw argument string against the command's nargs spec.
func nargsOK(nargs, args string) error {
	fields := strings.Fields(args)
	switch nargs {
	case "":
		if len(fields) != 0 {
			return fmt.Errorf(":%s takes no arguments", args)
		}
	case "?":
		if len(fields) > 1 {
			return fmt.Errorf(":%s takes at most one argument", args)
		}
	case "+":
		if len(fields) == 0 {
			return fmt.Errorf(":%s requires an argument", args)
		}
	}
	return nil
}

func (h *Host) commandNamespace(L *glua.LState) *glua.LTable {
	tb := L.NewTable()

	tb.RawSetString("add", L.NewFunction(func(L *glua.LState) int {
		if h.refuseDuringRender(L, "zemi.command.add") {
			return 0
		}
		name := L.CheckString(1)
		fn := L.CheckFunction(2)
		cmd := &Command{
			Name:   name,
			Fn:     fn,
			Origin: h.RT.CurrentOrigin(),
			Path:   h.RT.CurrentPath(),
			Gen:    h.currentGen(),
		}
		if L.GetTop() >= 3 && L.Get(3) != glua.LNil {
			opts, ok := L.Get(3).(*glua.LTable)
			if !ok {
				L.RaiseError("zemi.command.add: opts must be a table")
				return 0
			}
			if d, ok := opts.RawGetString("desc").(glua.LString); ok {
				cmd.Desc = string(d)
			}
			if n, ok := opts.RawGetString("nargs").(glua.LString); ok {
				if !validNargs(string(n)) {
					L.RaiseError("zemi.command.add: invalid nargs %q (want one of \"\", \"?\", \"+\", \"*\")", string(n))
					return 0
				}
				cmd.NArgs = string(n)
			}
			if c, ok := opts.RawGetString("complete").(*glua.LFunction); ok {
				cmd.Complete = c
			}
		}
		if _, err := h.Commands.Add(cmd); err != nil {
			L.RaiseError("zemi.command.add: %v", err)
			return 0
		}
		return 0
	}))

	tb.RawSetString("list", L.NewFunction(func(L *glua.LState) int {
		out := L.NewTable()
		for _, c := range h.Commands.List() {
			row := L.NewTable()
			row.RawSetString("name", glua.LString(c.Name))
			row.RawSetString("desc", glua.LString(c.Desc))
			row.RawSetString("nargs", glua.LString(c.NArgs))
			row.RawSetString("origin", glua.LString(c.Origin))
			out.Append(row)
		}
		L.Push(out)
		return 1
	}))

	return tb
}

// RunCommand executes a cmdline command by name. The reserved :Reload is
// never routed here (the UI dispatches it before Lua-visible commands).
// CompleteCommand returns argument candidates for a command line from the
// command's Go completer or its Lua complete(arg_lead, cmdline). Results are
// sanitized and capped; a failing completer offers nothing.
func (h *Host) CompleteCommand(ctx context.Context, name, lead, line string) []string {
	cmd := h.Commands.Lookup(name)
	if cmd == nil {
		return nil
	}
	var out []string
	switch {
	case cmd.GoComplete != nil:
		out = cmd.GoComplete(h, lead, line)
	case cmd.Complete != nil:
		res, err := h.RT.Call(ctx, cmd.Origin, cmd.Complete, lead, line)
		if err != nil || len(res) == 0 {
			return nil
		}
		list, _ := res[0].([]any)
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	if len(out) > 50 {
		out = out[:50]
	}
	for i := range out {
		out[i] = sanitize.String(out[i])
	}
	return out
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

func (h *Host) RunCommand(ctx context.Context, name, argString string) error {
	cmd := h.Commands.Lookup(name)
	if cmd == nil {
		return fmt.Errorf("not an editor command: %s", name)
	}
	if cmd.GoFn != nil {
		return cmd.GoFn(h, argString)
	}
	return h.runLuaCommand(ctx, cmd, argString)
}

func (h *Host) runLuaCommand(ctx context.Context, cmd *Command, argString string) error {
	if err := nargsOK(cmd.NArgs, argString); err != nil {
		return err
	}
	_, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
		h.RT.PushFrame(cmd.Origin, cmd.Path)
		defer h.RT.PopFrame()
		base := L.GetTop()
		L.Push(cmd.Fn)
		args := L.NewTable()
		args.RawSetString("args", glua.LString(argString))
		list := L.NewTable()
		for i, f := range strings.Fields(argString) {
			list.RawSetInt(i+1, glua.LString(f))
		}
		args.RawSetString("list", list)
		L.Push(args)
		if err := L.PCall(1, 0, nil); err != nil {
			L.SetTop(base)
			return nil, err
		}
		return nil, nil
	})
	return err
}

// RunRHS executes a keymap right-hand side on the ui class, attributed to
// the binding's registration origin (DESIGN §9 attribution rule). Host
// actions run on the caller's goroutine (never the interpreter), so config
// watches they fire go back through the queue.
func (h *Host) RunRHS(ctx context.Context, origin lua.Origin, rhs RHS, argString string) error {
	switch {
	case rhs.Fn != nil:
		_, err := h.RT.Do(ctx, lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			h.RT.PushFrame(origin, "")
			defer h.RT.PopFrame()
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
		return h.RunCommand(ctx, rhs.Command, argString)
	case rhs.API != "":
		fn, ok := h.hostActions[rhs.API]
		if !ok || fn == nil {
			return ErrReserved
		}
		return fn(h, argString)
	}
	return nil
}
