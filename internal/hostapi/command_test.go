package hostapi

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/lua"
)

// TestCompleteCommand: a Lua command's complete(arg_lead, cmdline) feeds
// argument candidates, narrowed and ranked by the lead like a Go
// completer's; Go subcommand completers switch lists by position.
func TestCompleteCommand(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		zemi.command.add("greet", function() end, {
			nargs = "*",
			complete = function(lead, line) return { "alice", { text = "bob", desc = "builder" }, lead } end,
		})
		zemi.command.add("boom", function() end, { nargs = "*", complete = function() error("nope") end })
	`)
	if got := fmt.Sprint(h.CompleteCommand(context.Background(), "greet", "b", "/greet b")); got != "[{b } {bob builder}]" {
		t.Fatalf("lua complete = %s", got)
	}
	if got := h.CompleteCommand(context.Background(), "boom", "", "/boom "); got != nil || !strings.Contains(h.errs.entries[0].msg, "nope") {
		t.Fatalf("failing completer = %v, errors %+v", got, h.errs.entries)
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

func TestMatch(t *testing.T) {
	cmds := []string{"reflect", "reload", "resume", "memory", "score"}
	for _, c := range []struct {
		name, query   string
		labels, texts []string
		rank          int // keep the best rank hits, best first; 0 keeps row order
		want          string
	}{
		{"an empty query keeps every row, no highlight", " ", cmds, nil, 0, "0[] 1[] 2[] 3[] 4[]"},
		{"a one-token label matches fuzzily", "cfg", []string{"config", "rewind"}, nil, 0, "0[0 3 5]"},
		{"a prefix hit highlights the prefix", "APP", []string{"app_app.go"}, nil, 0, "0[0 1 2]"},
		{"prose needs the word in it", "rsm", []string{"resume a session", "rsm"}, nil, 0, "1[0 1 2]"},
		{"padding makes no prose", "snt", []string{" sonnet "}, nil, 0, "0[1 3 6]"},
		{"every word, any order, any case", "two b", []string{"alpha one", "Beta two", "gamma three"}, nil, 0, "1[0 5 6 7]"},
		{"a word may match the text instead", "openai gpt", []string{"gpt-5", "o3"}, []string{"openai", "openai"}, 0, "0[0 1 2]"},
		{"a full id finds its row", "openai/gpt-5", []string{"anthropic/claude-sonnet-4", "openai/gpt-5"}, nil, 0, "1[0 1 2 3 4 5 6 7 8 9 10 11]"},
		{"runes match whole", "é", []string{"ã©.go", "café"}, nil, 0, "1[3]"},
		{"ranked: prefix hits by length, then score", "re", cmds, nil, 5, "1[0 1] 2[0 1] 0[0 1] 4[3 4]"},
		{"ranked and capped", "re", cmds, nil, 2, "1[0 1] 2[0 1]"},
	} {
		hits := Match(c.query, c.labels, c.texts)
		if c.rank > 0 {
			hits = Rank(hits, c.labels, c.rank)
		}
		var got []string
		for _, h := range hits {
			got = append(got, fmt.Sprintf("%d%v", h.Index, h.Pos))
		}
		if strings.Join(got, " ") != c.want {
			t.Errorf("%s: %q got %q, want %q", c.name, c.query, strings.Join(got, " "), c.want)
		}
	}
}

// TestNargsOK: a Go command's arguments are checked as typed and through
// zemi.command.run, so `/new idea` is not a plain /new.
func TestNargsOK(t *testing.T) {
	h, r := newTestHost(t)
	for _, c := range []struct{ nargs, args, want string }{
		{"", "", ""},
		{"", "bar", "/foo takes no arguments"},
		{"?", "a b", "/foo takes at most one argument"},
		{"+", " ", "/foo requires an argument"},
		{"*", "a b c", ""},
	} {
		h.Commands.Add(&Command{Name: "foo", NArgs: NArgs(c.nargs), GoFn: func(*Host, string) error { return nil }, Origin: lua.OriginBuiltin, Gen: h.currentGen()})
		got := ""
		if err := h.RunCommand(context.Background(), "foo", c.args); err != nil {
			got = err.Error()
		}
		if got != c.want {
			t.Errorf("/foo %q with nargs %q = %q, want %q", c.args, c.nargs, got, c.want)
		}
		runUser(t, r, fmt.Sprintf(`local _, err = zemi.command.run("foo", %q); assert((err or "") == %q, err)`, c.args, c.want))
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
	h.Commands.Add(&Command{Name: "stale", Complete: fn, Origin: lua.OriginUser, Gen: cmd.Gen})
	if got := h.CompleteCommand(context.Background(), "stale", "", "/stale "); got != nil {
		t.Errorf("stale complete = %q", got)
	}
	if called != 2 {
		t.Fatalf("stale function ran: called=%d", called)
	}
}

// TestCommandFailed: an error that is how the command was asked warns, one
// wrapped on the way up included; any other did not happen and is recorded.
func TestCommandFailed(t *testing.T) {
	h, _ := newTestHost(t)
	for _, c := range []struct {
		err      error
		level    string
		recorded bool
	}{
		{fmt.Errorf("resume: %w", Usagef("no session %q", "x")), "warn", false},
		{errors.New("open /nonexistent/x.md: no such file or directory"), "error", true},
	} {
		h.CommandFailed("x", c.err)
		n := h.evlog.notes[len(h.evlog.notes)-1]
		if n.level != c.level || n.body != c.err.Error() || h.Recorded(c.err.Error()) != c.recorded {
			t.Errorf("%v: toast %+v, recorded %v", c.err, n, h.Recorded(c.err.Error()))
		}
	}
}
