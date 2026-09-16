package hostapi

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
	glua "github.com/yuin/gopher-lua"
)

// TestCompleteCommand: a Lua command's complete(arg_lead, cmdline) feeds
// argument candidates; Go subcommand completers switch lists by position.
func TestCompleteCommand(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `zemi.command.add("greet", function() end, {
		nargs = "*",
		complete = function(lead, line) return { "alice", "bob", lead } end,
	})`)
	if got := h.CompleteCommand(context.Background(), "greet", "a", "/greet a"); strings.Join(got, ",") != "alice,bob,a" {
		t.Fatalf("lua complete = %q", got)
	}

	comp := subcommandCompleter([]string{"enable", "disable"}, func(*Host, string) []string { return []string{"lint"} })
	for _, c := range []struct{ lead, line, want string }{
		{"", "/skills ", "enable,disable"},
		{"en", "/skills en", "enable,disable"},
		{"", "/skills enable ", "lint"},
		{"li", "/skills enable li", "lint"},
	} {
		if got := strings.Join(comp(h, c.lead, c.line), ","); got != c.want {
			t.Errorf("%q: got %q, want %q", c.line, got, c.want)
		}
	}
}

func TestNargsOK(t *testing.T) {
	for _, c := range []struct{ nargs, args, want string }{
		{"", "", ""},
		{"", "bar", "/foo takes no arguments"},
		{"?", "a b", "/foo takes at most one argument"},
		{"+", " ", "/foo requires an argument"},
		{"*", "a b c", ""},
	} {
		got := ""
		if err := nargsOK("foo", c.nargs, c.args); err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("nargsOK(%q, %q) = %q, want %q", c.nargs, c.args, got, c.want)
		}
	}
}

// TestStaleKeymapAndCommandDoNotRun: a binding or command looked up before a
// reload must not run its function on the fresh LState.
func TestStaleKeymapAndCommandDoNotRun(t *testing.T) {
	h, r := newTestHost(t)
	called := 0
	var fn *glua.LFunction
	if _, err := r.Do(context.Background(), lua.ClassIO, 0, 0, func(L *glua.LState) ([]any, error) {
		fn = L.NewFunction(func(*glua.LState) int { called++; return 0 })
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	b := &KeyBinding{Key: "x", RHS: RHS{Fn: fn}, Origin: lua.OriginUser, Gen: h.currentGen()}
	cmd := &Command{Name: "x", Fn: fn, NArgs: "*", Origin: lua.OriginUser, Gen: h.currentGen()}
	run := func() []error {
		return []error{h.RunRHS(context.Background(), b), h.runLuaCommand(context.Background(), cmd, "")}
	}
	if errs := run(); errs[0] != nil || errs[1] != nil || called != 2 {
		t.Fatalf("live: errs=%v called=%d", errs, called)
	}
	if err := r.ForceReload(context.Background(), []lua.BootStep{h.NamespacesStep()}, h.PurgeGeneration); err != nil {
		t.Fatal(err)
	}
	for _, err := range run() {
		if !errors.Is(err, lua.ErrReloaded) {
			t.Errorf("stale run: %v", err)
		}
	}
	if called != 2 {
		t.Fatalf("stale function ran: called=%d", called)
	}
}
