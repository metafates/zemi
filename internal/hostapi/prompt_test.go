package hostapi

import (
	"cmp"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/tools"
)

// TestInstructionBudget: the budget is spent deepest file first, so the
// most specific file is whole and the general one is the one named as cut
// or left out.
func TestInstructionBudget(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	dir := t.TempDir()
	big, small := filepath.Join(dir, "BIG.md"), filepath.Join(dir, "SMALL.md")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.instructionSections([]string{big, small}, 100)
	if !strings.Contains(got, "\nsecond") || !strings.Contains(got, "(cut here: 106 of 200 bytes left out") {
		t.Fatalf("deepest file not kept whole: %q", got)
	}
	if got := h.instructionSections([]string{big, small}, 6); !strings.Contains(got, "BIG.md\n(left out: no room for its 200 bytes") {
		t.Fatalf("skipped file not named: %q", got)
	}
}

// TestUserInstructionsOwnBudget: a repository AGENTS.md over the budget is
// cut, and the user's own file still ships whole.
func TestUserInstructionsOwnBudget(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.Dirs.ConfigHome = t.TempDir()
	h.SetProjectTrusted(true)
	if err := os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "AGENTS.md"), []byte("my rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte(strings.Repeat("a", _contextBudget+100)), 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.systemText("base", nil, "", false)
	if !strings.Contains(got, "my rules") {
		t.Fatal("the repository file evicted the user's own instructions")
	}
	if !strings.Contains(got, "cut here") {
		t.Fatal("the repository file was not cut to the budget")
	}
}

// TestHomeInstructions: the cross-tool ~/.config/AGENTS.md rides the prompt
// whatever the project's trust, ahead of zemi's own <config>/AGENTS.md.
func TestHomeInstructions(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.Dirs.Home = t.TempDir()
	h.Dirs.ConfigHome = t.TempDir()
	if err := os.MkdirAll(filepath.Join(h.Dirs.Home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.Home, ".config", "AGENTS.md"), []byte("global rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "AGENTS.md"), []byte("zemi rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.systemText("base", nil, "", false)
	if !strings.Contains(got, "global rules") || !strings.Contains(got, "zemi rules") {
		t.Fatalf("the prompt misses the user's own instructions: %.300q", got)
	}
	if i, j := strings.Index(got, "global rules"), strings.Index(got, "zemi rules"); i >= j {
		t.Errorf("the cross-tool file at %d should precede zemi's own at %d", i, j)
	}
}

// TestPromptPinned: two sends around a change carry the same system prompt
// and tools unless the change is a drop point or moves the key, which an
// agent.assemble counter shows. A hand edit of an instruction file rides
// the next message once instead. The prompt starts with an AGENTS.md, or
// with the file a row's setup names.
func TestPromptPinned(t *testing.T) {
	agentsMD := func(h *Host) string { return filepath.Join(h.Dirs.ProjectDir, "AGENTS.md") }
	handEdit := func(h *Host, path, text string) {
		os.WriteFile(path, []byte(text), 0o644)
		later := time.Now().Add(time.Hour)
		os.Chtimes(path, later, later)
	}
	for _, c := range []struct {
		name    string
		setup   string
		between func(t *testing.T, h *Host, r *lua.Runtime)
		rebuilt bool
		attach  int
	}{
		{"AGENTS.md edited by hand", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			handEdit(h, agentsMD(h), "run make check")
		}, false, 1},
		{"AGENTS.md created by hand next to a CLAUDE.md", "CLAUDE.md", func(t *testing.T, h *Host, _ *lua.Runtime) {
			handEdit(h, agentsMD(h), "run make check")
		}, false, 1},
		{"AGENTS.md edited by hand once trust is off", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			handEdit(h, agentsMD(h), "run make check")
			h.SetProjectTrusted(false)
		}, true, 0},
		{"hand edit rides the next message once", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			handEdit(h, agentsMD(h), "run make check")
			sender(t, h)("mid")
		}, false, 0},
		{"AGENTS.md edited through the edit tool", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			all := tools.All(h.PipelineFor())
			for _, call := range []struct {
				name string
				in   map[string]any
			}{
				{"read", map[string]any{"path": agentsMD(h), "limit": 0}},
				{"edit", map[string]any{"path": agentsMD(h), "search": "test", "replace": "check"}},
			} {
				i := slices.IndexFunc(all, func(a fantasy.AgentTool) bool { return a.Info().Name == call.name })
				input, _ := json.Marshal(call.in, json.Deterministic(true))
				if resp, err := all[i].Run(t.Context(), fantasy.ToolCall{ID: "t", Name: call.name, Input: string(input)}); err != nil || resp.IsError {
					t.Fatalf("%s: %v %s", call.name, err, resp.Content)
				}
			}
		}, false, 0},
		{"skill added", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			writeSkill(t, h.userSkillsDir(), "fresh", "a fresh skill", "")
			h.syncSkills()
		}, false, 0},
		{"workflow saved", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			os.MkdirAll(filepath.Join(h.Dirs.ConfigHome, "workflows"), 0o755)
			os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "workflows", "fresh.lua"), []byte("-- a fresh workflow\nreturn 1\n"), 0o644)
		}, false, 0},

		{"assemble handler changed and reloaded", "", func(t *testing.T, h *Host, r *lua.Runtime) {
			if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
				t.Fatal(err)
			}
			runUser(t, r, `zemi.event.on("agent.assemble", function() return "changed" end)`)
		}, false, 0},
		{"provider refused images", "", func(t *testing.T, _ *Host, _ *lua.Runtime) {
			_imagesRejected.Store("stub/m", true)
			t.Cleanup(func() { _imagesRejected.Delete("stub/m") })
		}, true, 0},
		{"model switch", "", func(t *testing.T, _ *Host, r *lua.Runtime) {
			runUser(t, r, `zemi.config.set("models.default", "stub/m2")`)
		}, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			h.Dirs.ConfigHome = t.TempDir()
			h.SetProjectTrusted(true)
			h.Settings.SetQuiet("models.default", "stub/m")
			m := replyStub(t, h, "ok")
			os.WriteFile(filepath.Join(h.Dirs.ProjectDir, cmp.Or(c.setup, "AGENTS.md")), []byte("run make test"), 0o644)
			runUser(t, r, `local n = 0
zemi.event.on("agent.assemble", function() n = n + 1; return "build " .. n end)`)
			send := sender(t, h)
			send("one")
			c.between(t, h, r)
			send("two")
			m.mu.Lock()
			first, last := m.prompts[0], m.prompts[len(m.prompts)-1]
			tools0, _ := json.Marshal(m.tools[0], json.Deterministic(true))
			tools1, _ := json.Marshal(m.tools[len(m.tools)-1], json.Deterministic(true))
			m.mu.Unlock()
			sys0, sys1 := concatText(first[0].Content), concatText(last[0].Content)
			if rebuilt := strings.Contains(sys1, "build 2"); rebuilt != c.rebuilt {
				t.Errorf("rebuilt = %v, want %v", rebuilt, c.rebuilt)
			}
			if !c.rebuilt && (sys0 != sys1 || string(tools0) != string(tools1)) {
				t.Errorf("the prefix moved: prompt equal %v, tools equal %v", sys0 == sys1, string(tools0) == string(tools1))
			}
			msg := concatText(last[len(last)-1].Content)
			if n := strings.Count(msg, `<instructions path="AGENTS.md">`); n != c.attach || c.attach > 0 && !strings.Contains(msg, "run make check") {
				t.Errorf("%d attachments, want %d: %q", n, c.attach, msg)
			}
		})
	}
}

// buildSystem renders the system prompt with its context message joined.
func (h *Host) buildSystem() string {
	toolset := h.mainTools()
	sys, ctx, _ := h.buildSystemParts(toolset)
	return sys + "\n\n" + ctx
}

// systemText is what systemWith sends before the conversation, joined.
func (h *Host) systemText(sys string, toolset []fantasy.AgentTool, pad string, readOnly bool) string {
	sys, ctx := h.systemWith(sys, toolset, pad, readOnly)
	return sys + "\n\n" + ctx
}
