package hostapi

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
)

const rolesProvider = `zemi.provider.add({ id = "a", kind = "openai_compat", base_url = "http://x", models = { { id = "big" }, { id = "small" } } })
`

// TestRoleModel: an empty role follows models.chat, which follows the
// first provider's default; a profile's model moves the conversation only
// and may name a role; a models.* key refuses a role name.
func TestRoleModel(t *testing.T) {
	for _, c := range []struct {
		name, config string
		want         []string // the conversation, then chat, agents, titles, compact, memory
		err          string
	}{
		{"nothing set runs the provider default", ``, []string{"a/big", "a/big", "a/big", "a/big", "a/big", "a/big"}, ""},
		{"unset roles follow chat", `zemi.config.set("models.chat", "a/small")`, []string{"a/small", "a/small", "a/small", "a/small", "a/small", "a/small"}, ""},
		{"a set role wins", `zemi.config.set("models.titles", "a/small") zemi.config.set("models.memory", "a/small")`, []string{"a/big", "a/big", "a/big", "a/small", "a/big", "a/small"}, ""},
		{"a profile's model moves the conversation only", `zemi.agent.profile.add("p", { model = "a/small" }) zemi.config.set("agent.profile", "p")`, []string{"a/small", "a/big", "a/big", "a/big", "a/big", "a/big"}, ""},
		{"a profile names a role", `zemi.config.set("models.titles", "a/small") zemi.agent.profile.add("p", { model = "titles" }) zemi.config.set("agent.profile", "p")`, []string{"a/small", "a/big", "a/big", "a/small", "a/big", "a/big"}, ""},
		{"a key refuses a role name", `zemi.config.set("models.titles", "chat")`, nil, `config key models.titles expects a provider/model id or ""`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHost(t)
			if c.err != "" {
				runExpectErr(t, r, rolesProvider+c.config, c.err)
				return
			}
			runUser(t, r, rolesProvider+c.config)
			p, _ := h.resolveProfile("")
			if got := append([]string{h.currentModelID(p)}, h.roleIDs()...); !slices.Equal(got, c.want) {
				t.Fatalf("roles run on %v, want %v", got, c.want)
			}
		})
	}
}

// TestSetModelArgs: /model's grammar. The cases share one host, in order.
func TestSetModelArgs(t *testing.T) {
	h, r := newTestHost(t)
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	runUser(t, r, rolesProvider+`zemi.config.set("models.chat", "a/big")`)
	for _, c := range []struct {
		args   string
		opened string // the overlay it opens
		key    string // else the key it writes
		want   string
		err    string
	}{
		{"", "model", "", "", ""},
		{"chat", "model chat", "", "", ""},
		{"roles", "model roles", "", "", ""},
		{"titles", "model titles", "", "", ""},
		{"a/small", "", "models.chat", "a/small", ""},
		{"titles a/big", "", "models.titles", "a/big", ""},
		{"titles chat", "", "models.titles", "", ""},
		{"titles nope/x", "", "", "", `provider "nope" is not registered`},
		{"fast a/big", "", "", "", "usage: /model"},
		{"titles a/big more", "", "", "", "usage: /model"},
	} {
		n := len(ui.opened)
		err := h.SetModel(c.args)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: err %v, want %q", c.args, err, c.err)
			}
		case err != nil:
			t.Errorf("%q: %v", c.args, err)
		case c.opened != "":
			if len(ui.opened) != n+1 || ui.opened[n].ID != c.opened {
				t.Errorf("%q opened %v, want %q", c.args, ui.opened[n:], c.opened)
			}
		case h.Settings.Str(c.key) != c.want:
			t.Errorf("%q: %s = %q, want %q", c.args, c.key, h.Settings.Str(c.key), c.want)
		case len(ui.opened) != n+1 || ui.opened[n].ID != "model" || !ui.opened[n].Refresh:
			t.Errorf("%q refreshed %v, want an open /model", c.args, ui.opened[n:])
		}
	}
	// the roles list tags what follows chat, and zemi.model.list says what runs where
	roles := ui.opened[2].Items
	if k := roles[2].(map[string]any)["key"]; k != "follows chat" {
		t.Errorf("titles row key %v", k)
	}
	for _, c := range []struct{ line, want string }{
		{"/model ", "roles update chat agents titles compact memory a/big a/small"},
		{"/model titles ", "chat a/big a/small"},
		{"/model chat ", "a/big a/small"},
		{"/model a/big ", ""},
	} {
		// builtins an exported key turns on are left out
		got := slices.DeleteFunc(h.CompleteModel("", c.line), func(s string) bool { return strings.Contains(s, "/") && !strings.HasPrefix(s, "a/") })
		if strings.Join(got, " ") != c.want {
			t.Errorf("complete %q = %v, want %q", c.line, got, c.want)
		}
	}
	runUser(t, r, `zemi.config.set("models.memory", "a/big")
		for _, m in ipairs(zemi.model.list()) do
			local want = m.id == "a/big" and "memory" or "chat,agents,titles,compact"
			assert(table.concat(m.roles, ",") == want, m.id .. ": " .. table.concat(m.roles, ","))
		end`)
}

// TestInertBoot: an inert boot loads what config registers and does none of
// its IO but reads: no MCP start, no job, no model call, no file sync.
func TestInertBoot(t *testing.T) {
	project, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "f.txt"), []byte("read"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: project, DataHome: data})
	h.Inert = true
	h.modelFn = func() (fantasy.LanguageModel, error) {
		t.Error("an inert boot called a model")
		return nil, errors.New("no")
	}
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `
		zemi.mcp.add("loud", { command = "/nonexistent/loud" })
		assert(zemi.fs.read("f.txt") == "read")
		local ok, err = pcall(zemi.job.start, { cmd = "true" })
		assert(not ok and tostring(err):find("permission denied (inert)", 1, true), tostring(err))
		local sent, why = zemi.agent.send("hi")
		assert(sent == nil and why, "send went through")
		zemi.provider.add({ id = "p", kind = "openai_compat", base_url = "http://x", models = { { id = "m" } } })
	`)
	if s := h.mcp.servers["loud"]; s == nil || s.status != StatusOff {
		t.Errorf("mcp server %+v, want registered and off", s)
	}
	if _, ok := h.Providers.Lookup("p"); !ok {
		t.Error("the provider after the refused calls is missing")
	}
	if _, err := os.Stat(h.TypesDir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the types were synced: %v", err)
	}
}
