package hostapi

import (
	"context"
	"strings"
	"testing"
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
