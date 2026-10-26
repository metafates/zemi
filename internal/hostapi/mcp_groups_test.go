// mcp_groups_test.go pins the group toggles: the switch semantics, the
// persistence in the global store's kv, the name collisions, and the /mcp
// tree layout.

package hostapi

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/mcp"
	"github.com/metafates/zemi/internal/mcp/mcptest"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// mcpFakeServer is an HTTP fake MCP server listing the mcptest tools.
func mcpFakeServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m struct {
			ID     jsontext.Value `json:"id"`
			Method string         `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		if resp := mcptest.Respond(body); resp != nil {
			if m.Method == "initialize" {
				w.Header().Set("Mcp-Session-Id", "fake-session")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(resp)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// addFakeMCP registers one fake server and waits for it to be ready or off.
func addFakeMCP(t *testing.T, h *Host, r *lua.Runtime, name, group string) {
	t.Helper()
	runUser(t, r, fmt.Sprintf(`zemi.mcp.add(%q, { url = %q, group = %q })`, name, mcpFakeServer(t), group))
	waitFor(t, func() error {
		s, err := h.mcpServerNamed(name)
		if err != nil {
			return err
		}
		if s.currentClient() == nil && mcpStatus(s) != StatusOff {
			return errors.New("still not up")
		}
		return nil
	})
}

// serverOf resolves a server and fails when it is missing.
func serverOf(t *testing.T, h *Host, name string) *mcpServer {
	t.Helper()
	s, err := h.mcpServerNamed(name)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mcpStatus reads a server's status under its lock.
func mcpStatus(s *mcpServer) ServiceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// evalLua runs a user chunk and returns its first return value.
func evalLua(t *testing.T, r *lua.Runtime, src string) any {
	t.Helper()
	vals, err := runChunk(t, r, src)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	if len(vals) == 0 {
		return nil
	}
	return vals[0]
}

func TestMCPToggle(t *testing.T) {
	t.Parallel()

	t.Run("disable stops a live server and persists", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "solo", "")
		s := serverOf(t, h, "solo")
		done := s.currentClient().Done()
		if err := h.toggleMCPName("solo"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() error {
			select {
			case <-done:
				return nil
			default:
				return errors.New("the replaced client is still open")
			}
		})
		s = serverOf(t, h, "solo")
		if mcpStatus(s) != StatusOff || s.enabledFlag() {
			t.Fatalf("status %q enabled %v, want off", mcpStatus(s), s.enabledFlag())
		}
		if v, err := h.GlobalDB.GetKV("mcp:solo"); err != nil || v != "false" {
			t.Fatalf("kv = %q, %v, want \"false\"", v, err)
		}
		for _, o := range h.mcpOffered() {
			if o.srv.name == "solo" {
				t.Fatal("a disabled server still offers tools")
			}
		}
	})

	t.Run("enable starts it again and clears the row", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "solo", "")
		if err := h.setMCPName("solo", false); err != nil {
			t.Fatal(err)
		}
		if err := h.toggleMCPName("solo"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() error {
			if s := serverOf(t, h, "solo"); mcpStatus(s) != StatusReady {
				return fmt.Errorf("still %q", mcpStatus(s))
			}
			return nil
		})
		s := serverOf(t, h, "solo")
		if !s.enabledFlag() {
			t.Fatal("the enabled server kept its off switch")
		}
		if _, err := h.GlobalDB.GetKV("mcp:solo"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("kv err = %v, want not found", err)
		}
		var tools int
		for _, o := range h.mcpOffered() {
			if o.srv.name == "solo" {
				tools++
			}
		}
		if tools == 0 {
			t.Fatal("an enabled server offers no tools")
		}
	})

	t.Run("enabling a ready server restarts nothing", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "solo", "")
		s := serverOf(t, h, "solo")
		client := s.currentClient()
		if err := h.setMCPName("solo", true); err != nil {
			t.Fatal(err)
		}
		if serverOf(t, h, "solo").currentClient() != client {
			t.Fatal("an enable on a ready server restarted it")
		}
	})

	t.Run("the switch survives a re-add", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "solo", "")
		s := serverOf(t, h, "solo")
		if err := h.setMCPName("solo", false); err != nil {
			t.Fatal(err)
		}
		// a boot re-registers the server with its spec alone
		if err := h.addMCP("solo", s.spec); err != nil {
			t.Fatal(err)
		}
		if s := serverOf(t, h, "solo"); mcpStatus(s) != StatusOff {
			t.Fatalf("a re-added server came up %q, want off", mcpStatus(s))
		}
	})

	t.Run("a mixed group comes on together, an all-on one goes off together", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "jira", "work")
		addFakeMCP(t, h, r, "wiki", "work")
		if err := h.setMCPName("wiki", false); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() error {
			if s := serverOf(t, h, "wiki"); mcpStatus(s) != StatusOff {
				return errors.New("the off state has not settled")
			}
			return nil
		})
		if err := h.toggleMCPName("work"); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"jira", "wiki"} {
			waitFor(t, func() error {
				if s := serverOf(t, h, name); !s.enabledFlag() || mcpStatus(s) != StatusReady {
					return fmt.Errorf("%s: status %q, want ready after the group came on", name, mcpStatus(s))
				}
				return nil
			})
		}
		if err := h.toggleMCPName("work"); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"jira", "wiki"} {
			waitFor(t, func() error {
				if s := serverOf(t, h, name); mcpStatus(s) != StatusOff {
					return fmt.Errorf("%s still %q", name, mcpStatus(s))
				}
				return nil
			})
		}
	})

	t.Run("a group toggle leaves ungrouped servers alone", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "jira", "work")
		addFakeMCP(t, h, r, "solo", "")
		if err := h.toggleMCPName("work"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() error {
			if s := serverOf(t, h, "jira"); mcpStatus(s) != StatusOff {
				return errors.New("the group member is still on")
			}
			return nil
		})
		if s := serverOf(t, h, "solo"); !s.enabledFlag() {
			t.Fatal("an ungrouped server followed the group")
		}
	})

	t.Run("a name is a server or a group, never both", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "jira", "work")
		url := mcpFakeServer(t)
		for _, c := range []struct {
			name  string
			group string
			want  string
		}{
			{"work", "", `server name "work" is already a group's name`},
			{"b", "jira", `group "jira" is already a server's name`},
			{"c", "c", `group "c" cannot be the server's own name`},
		} {
			if err := h.addMCP(c.name, mcpSpec{URL: url, Group: c.group}); err == nil || err.Error() != c.want {
				t.Fatalf("add(%s, group %q) = %v, want %q", c.name, c.group, err, c.want)
			}
		}
	})

	t.Run("the kv row is parsed, never compared", func(t *testing.T) {
		h, r := newTestHostDB(t)
		if err := h.GlobalDB.SetKV("mcp:solo", `"false"`); err != nil { // a JSON string, as user Lua writes it
			t.Fatal(err)
		}
		addFakeMCP(t, h, r, "solo", "")
		if s := serverOf(t, h, "solo"); !s.enabledFlag() || mcpStatus(s) != StatusReady {
			t.Fatalf("status %q enabled %v, want the quoted row ignored", mcpStatus(s), s.enabledFlag())
		}
	})

	t.Run("a group move adopts the server without a restart", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "jira", "work")
		s := serverOf(t, h, "jira")
		client := s.currentClient()
		spec := s.specSnapshot()
		spec.Group = "db"
		// a reload replays the adds over a parked generation
		h.mcp.park()
		if err := h.addMCP("jira", spec); err != nil {
			t.Fatal(err)
		}
		s = serverOf(t, h, "jira")
		if s.currentClient() != client {
			t.Fatal("a group move restarted the server")
		}
		if s.group() != "db" {
			t.Fatalf("group %q, want db", s.group())
		}
		if mcpStatus(s) != StatusReady {
			t.Fatalf("status %q, want ready", mcpStatus(s))
		}
		h.mcp.park()
		h.mcp.stopParked()
	})

	t.Run("restarting a disabled server names the way out", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "solo", "")
		if err := h.setMCPName("solo", false); err != nil {
			t.Fatal(err)
		}
		err := h.restartMCP("solo")
		if err == nil || !strings.Contains(err.Error(), "is off") {
			t.Fatalf("err = %v, want the off hint", err)
		}
	})

	t.Run("a sign-in completing for a disabled server restarts nothing", func(t *testing.T) {
		h, r := newTestHostDB(t)
		addFakeMCP(t, h, r, "solo", "")
		if err := h.setMCPName("solo", false); err != nil {
			t.Fatal(err)
		}
		h.signedInMCP("solo")
		if s := serverOf(t, h, "solo"); mcpStatus(s) != StatusOff {
			t.Fatalf("status %q, want the disabled server left off", mcpStatus(s))
		}
	})

	t.Run("an unknown name names both", func(t *testing.T) {
		h, _ := newTestHostDB(t)
		for _, toggle := range []func(string) error{h.toggleMCPName, func(n string) error { return h.setMCPName(n, true) }} {
			if err := toggle("nope"); err == nil || err.Error() != `no MCP server or group "nope"` {
				t.Fatalf("err = %v, want the both-worded miss", err)
			}
		}
	})
}

func TestMCPToggleLua(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	addFakeMCP(t, h, r, "jira", "work")
	addFakeMCP(t, h, r, "solo", "")

	runUser(t, r, `zemi.mcp.enable("work", false)`)
	waitFor(t, func() error {
		if s := serverOf(t, h, "jira"); mcpStatus(s) != StatusOff {
			return errors.New("the group is still on")
		}
		return nil
	})

	rows, ok := evalLua(t, r, `
		local out = {}
		for _, row in ipairs(zemi.mcp.list()) do
			table.insert(out, { name = row.name, group = row.group, enabled = row.enabled })
		end
		return out
	`).([]any)
	if !ok {
		t.Fatalf("zemi.mcp.list() returned %#v", rows)
	}
	byName := map[string]map[string]any{}
	for _, row := range rows {
		m := row.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if g := byName["jira"]; g["group"] != "work" || g["enabled"] != false {
		t.Fatalf("jira = %v, want group work, disabled", g)
	}
	if g := byName["solo"]; g["group"] != "" || g["enabled"] != true {
		t.Fatalf("solo = %v, want ungrouped, enabled", g)
	}

	vals, err := runChunk(t, r, `return zemi.mcp.enable("nope", true)`)
	if err != nil || len(vals) != 2 || vals[0] != nil || vals[1] != `no MCP server or group "nope"` {
		t.Fatalf("vals = %v, err = %v, want nil and the both-worded miss", vals, err)
	}
}

func TestMCPGroupItems(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	addFakeMCP(t, h, r, "jira", "work")
	addFakeMCP(t, h, r, "wiki", "work")
	addFakeMCP(t, h, r, "solo", "")
	if err := h.setMCPName("wiki", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if s := serverOf(t, h, "wiki"); mcpStatus(s) != StatusOff {
			return errors.New("the off state has not settled")
		}
		return nil
	})

	items := h.mcpItems()
	if len(items) != 2 {
		t.Fatalf("%d items, want the group row and the solo server", len(items))
	}
	g := items[0]
	if g.ID != "mcp group work" || g.Text != "work" || g.Run != "mcp toggle work" {
		t.Fatalf("group row = %+v", g)
	}
	if g.On == nil || *g.On || g.Desc != "1 of 2 on" {
		t.Fatalf("group row switch = %v desc %q, want off, 1 of 2 on", g.On, g.Desc)
	}
	if len(g.Row) != 1 || g.Row[0].HL != "Title" || g.Row[0].Text != "work" {
		t.Fatalf("group row label = %+v, want the title-colored name", g.Row)
	}
	if len(g.Fields) != 0 {
		t.Fatalf("group row has fields %v: enter would open a view instead of toggling", g.Fields)
	}
	if len(g.Keys) != 3 || g.Keys[0].Key != "l" || g.Keys[0].Run != "mcp group work" || g.Keys[0].Verb != VerbOpen ||
		g.Keys[1].Key != "e" || g.Keys[2].Key != "d" {
		t.Fatalf("group row keys = %+v, want l open, enable all and disable all", g.Keys)
	}
	solo := items[1]
	if solo.Row != nil {
		t.Fatalf("an ungrouped row carries a tree line %v", solo.Row)
	}
	if solo.On == nil || !*solo.On || solo.Run != "mcp toggle solo" {
		t.Fatalf("solo row = %+v, want a switch and a toggle run", solo)
	}
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openMCPGroup("nope"); err == nil {
		t.Fatal("an unknown group opened a level")
	}
	if err := h.openMCPGroup(""); err == nil {
		t.Fatal("a groupless open opened a level")
	}
	if err := h.openMCPGroup("work"); err != nil {
		t.Fatal(err)
	}
	if got := ui.overlays(); len(got) != 1 || got[0].ID != "mcp group work" || got[0].Title != "MCP servers / work" {
		t.Fatalf("group level = %+v", ui.overlays())
	}
	o := ui.overlays()[0]
	if len(o.Items) != 2 || o.Items[0].ID != "mcp jira" || o.Items[0].Row != nil {
		t.Fatalf("level rows = %+v", o.Items)
	}
	if o.Reload == nil {
		t.Fatal("the level is not a toggle list")
	}
	row := o.Items[1]
	if row.ID != "mcp wiki" || *row.On || row.Desc != string(StatusOff) || row.Mark != tools.MarkStop {
		t.Fatalf("the disabled member row = %+v, want off switch, off desc, stop mark", row)
	}
	if slices.ContainsFunc(row.Keys, func(k ItemKey) bool { return k.Key == "r" }) {
		t.Fatal("a disabled server offers a restart")
	}
}

// TestMCPGroupLevelRefresh: a server's state moving off the key loop rebuilds
// the group level beside the list, and a member toggled inside the level
// shows in the list once it is uncovered.
func TestMCPGroupLevelRefresh(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	defer h.mcp.stopParked()
	defer h.mcp.park()
	addFakeMCP(t, h, r, "jira", "work")
	addFakeMCP(t, h, r, "solo", "")
	ui := &fakeUI{}
	h.SetUIHooks(ui)
	if err := h.openMCPGroup("work"); err != nil {
		t.Fatal(err)
	}
	h.setMCP(serverOf(t, h, "jira"), StatusFailed, "boom")
	ids, rebuilt := snapUI(ui)
	if !slices.Contains(ids, "mcp") || !slices.Contains(ids, "mcp group work") {
		t.Fatalf("refreshes = %v, want the list and the level", ids)
	}
	i := slices.IndexFunc(rebuilt, func(o Overlay) bool { return o.ID == "mcp group work" })
	if i < 0 {
		t.Fatalf("no level rebuild in %v", ids)
	}
	if row := rebuilt[i].Items[0]; row.ID != "mcp jira" || row.Desc != "failed" {
		t.Fatalf("the rebuilt level row = %+v, want the failed state", row)
	}
	if err := h.toggleMCPName("jira"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if s := serverOf(t, h, "jira"); s.enabledFlag() {
			return errors.New("the toggle has not settled")
		}
		return nil
	})
	items := h.mcpItems()
	if len(items) != 2 || items[0].Desc != "0 of 1 on" {
		t.Fatalf("list after the member toggle = %+v", items)
	}
}

// snapUI reads a fakeUI's refresh ids and rebuilt overlays under its lock.
func snapUI(ui *fakeUI) ([]string, []Overlay) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	return slices.Clone(ui.refreshed), slices.Clone(ui.rebuilt)
}

// plantGrant writes a stored grant for one server the way the OS keychain or
// its fallback file would hold it.
func plantGrant(t *testing.T, h *Host, s *mcpServer) {
	t.Helper()
	spec := s.specSnapshot()
	grant, err := jsonv2.Marshal(mcp.Token{
		ServerURL: spec.URL, AccessToken: "a0", ClientID: "c",
		AuthURL: spec.URL + "/a", TokenURL: spec.URL + "/t", RedirectURI: spec.URL + "/c",
	}, jsonv2.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(h.mcpAuth(s).Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.mcpAuth(s).Path, grant, 0o600); err != nil {
		t.Fatal(err)
	}
}

// signOutKey reports whether a server's row offers x.
func signOutKey(items []OverlayItem, name string) bool {
	i := slices.IndexFunc(items, func(it OverlayItem) bool { return it.ID == "mcp "+name })
	return i >= 0 && slices.ContainsFunc(items[i].Keys, func(k ItemKey) bool { return k.Key == "x" })
}

// TestMCPSignOutKey: the keychain is asked once per registration off the UI
// path, so the sign-out key follows the grant without a read on every build.
func TestMCPSignOutKey(t *testing.T) {
	t.Parallel()
	h, r := newTestHostDB(t)
	defer h.mcp.stopParked()
	defer h.mcp.park()
	addFakeMCP(t, h, r, "solo", "")
	if signOutKey(h.mcpItems(), "solo") {
		t.Fatal("a server with no grant offers the sign-out key")
	}
	plantGrant(t, h, serverOf(t, h, "solo"))
	if err := h.restartMCP("solo"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() error {
		if !signOutKey(h.mcpItems(), "solo") {
			return errors.New("the stored grant never showed the sign-out key")
		}
		return nil
	})
	if err := h.signOutMCP("solo", true); err != nil {
		t.Fatal(err)
	}
	if signOutKey(h.mcpItems(), "solo") {
		t.Fatal("the sign-out key stayed after signing out")
	}
	// the build never asks the keychain: a grant planted now waits for the
	// next registration instead of lighting the key mid-list
	plantGrant(t, h, serverOf(t, h, "solo"))
	if signOutKey(h.mcpItems(), "solo") {
		t.Fatal("a list build read the keychain")
	}
}
