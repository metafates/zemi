package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
)

// newTestHostSkills boots a host with stores plus skill directories and
// the path classes real boot installs (authored skills are user origin,
// learned skills learned origin).
func newTestHostSkills(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	h, r := newTestHostSkillsNoBoot(t)
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// newTestHostSkillsNoBoot is newTestHostSkills without the boot, for tests
// that need to lay down skill files first: every Boot builds a fresh
// generation, so all steps that must share state must pass in ONE call.
func newTestHostSkillsNoBoot(t *testing.T) (*Host, *lua.Runtime) {
	t.Helper()
	log := logging.New(t.TempDir())
	cfg, data, state, proj := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	classes := lua.PathClasses{
		{Prefix: cfg, Origin: lua.OriginUser},
		{Prefix: filepath.Join(data, "skills"), Origin: lua.OriginLearned},
	}
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, PathClass: classes})
	h := NewHost(r, log, config.NewStore(), config.Dirs{
		ConfigHome: cfg,
		DataHome:   data,
		StateHome:  state,
		ProjectDir: proj,
	})
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	return h, r
}

// writeSkill creates one skill directory with a SKILL.md and an optional
// zemi-skill.lua hook.
func writeSkill(t *testing.T, base, name, description, hook string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nBody.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if hook != "" {
		if err := os.WriteFile(filepath.Join(dir, "zemi-skill.lua"), []byte(hook), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSkillsQuarantineDefaults covers §10.5: authored skills start
// enabled, learned skills start disabled, and the skill index the model
// sees lists enabled skills only.
func TestSkillsQuarantineDefaults(t *testing.T) {
	h, r := newTestHostSkills(t)
	writeSkill(t, h.authoredSkillsDir(), "local-run", "run the local suite", "")
	writeSkill(t, h.learnedSkillsDir(), "auto-learned", "learned from sessions", "")
	h.syncSkills()

	runUser(t, r, `
		local all = zemi.skills.list()
		assert(#all == 2, "skills=" .. #all)
		local byName = {}
		for _, s in ipairs(all) do byName[s.name] = s end
		assert(byName["local-run"].enabled == true, "authored must be enabled")
		assert(byName["local-run"].source == "authored", "source=" .. tostring(byName["local-run"].source))
		assert(byName["auto-learned"].enabled == false, "learned must start disabled")
		assert(byName["auto-learned"].source == "learned", "source=" .. tostring(byName["auto-learned"].source))
	`)
	if n := len(h.skillIndex()); n != 1 {
		t.Fatalf("skillIndex=%d, want 1 (enabled only)", n)
	}
	if h.skillIndex()[0].Name != "local-run" {
		t.Fatalf("skillIndex=%+v", h.skillIndex()[0])
	}
}

// TestSkillsEnableGate covers the origin gate: user origin flips the
// state, a project-origin chunk is refused, unknown names are refused.
func TestSkillsEnableGate(t *testing.T) {
	h, r := newTestHostSkills(t)
	writeSkill(t, h.learnedSkillsDir(), "auto-learned", "learned", "")
	h.syncSkills()

	runUser(t, r, `zemi.skills.enable("auto-learned")`)
	runUser(t, r, `
		for _, s in ipairs(zemi.skills.list()) do
			if s.name == "auto-learned" then
				assert(s.enabled == true, "enable did not stick")
			end
		end
	`)

	err := r.ExecuteChunk(context.Background(), `zemi.skills.enable("auto-learned")`, "project.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "user or builtin origin") {
		t.Fatalf("project-origin enable: %v", err)
	}
	runExpectErr(t, r, `zemi.skills.disable("missing-skill")`, "no such skill")
}

// TestSkillsLearnedHookOnEnable covers the learned enable hook: it runs
// under the learned origin on enable (commands and widgets allowed), and
// a tool registration inside it is refused.
func TestSkillsLearnedHookOnEnable(t *testing.T) {
	h, r := newTestHostSkills(t)
	writeSkill(t, h.learnedSkillsDir(), "hooked", "learned with a hook", `
		zemi.command.add("hooked-cmd", function() end, { desc = "from skill hook" })
		local ok = pcall(zemi.tool.register, {
			name = "hooked-tool", description = "nope",
			execute = function() end })
		assert(not ok, "learned origin registered a tool")
	`)
	h.syncSkills()
	runUser(t, r, `zemi.skills.enable("hooked")`)
	if h.Commands.Lookup("hooked-cmd") == nil {
		t.Fatal("learned skill hook did not register its command")
	}

	writeSkill(t, h.learnedSkillsDir(), "hooked2", "go-side enable", `
		zemi.command.add("hooked2-cmd", function() end, { desc = "from skill hook" })
	`)
	h.syncSkills()
	if err := h.skillsCommand("enable hooked2"); err != nil {
		t.Fatalf(":skills enable: %v", err)
	}
	if h.Commands.Lookup("hooked2-cmd") == nil {
		t.Fatal("Go-side enable hook did not run")
	}
}

// TestSkillsAuthoredHooksStep covers the boot-window hook run: an enabled
// authored skill's zemi-skill.lua runs in the authored hooks step. The
// skill must exist before boot (production lays down dirs before boot).
func TestSkillsAuthoredHooksStep(t *testing.T) {
	h, r := newTestHostSkillsNoBoot(t)
	writeSkill(t, h.authoredSkillsDir(), "booted", "authored", `
		zemi.command.add("booted-cmd", function() end, { desc = "from authored hook" })
	`)
	h.syncSkills()
	if err := r.Boot(context.Background(), []lua.BootStep{
		h.NamespacesStep(), h.SkillsAuthoredHooksStep(),
	}); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if h.Commands.Lookup("booted-cmd") == nil {
		t.Fatal("authored hook did not run in the boot window")
	}
}

// TestLearnProposalsAndDecide covers the §6.7 surface: proposals mirrors
// the memory queue, decide accepts or rejects, and only a user or builtin
// origin may decide.
func TestLearnProposalsAndDecide(t *testing.T) {
	h, r := newTestHostSkills(t)
	seedSession(t, h)

	runUser(t, r, `zemi.memory.add({ kind = "fact", text = "deploy script lives in bin/deploy" })`)
	runUser(t, r, `
		local p = zemi.learn.proposals()
		assert(#p == 1, "proposals=" .. #p)
		assert(p[1].kind == "fact", "kind=" .. tostring(p[1].kind))
		assert(p[1].id ~= nil and p[1].id ~= "", "id missing")
	`)
	runUser(t, r, `zemi.learn.decide(zemi.learn.proposals()[1].id, "reject")`)
	runUser(t, r, `
		assert(#zemi.learn.proposals() == 0, "rejected proposal still queued")
	`)
	if rows := projectMemories(t, h); len(rows) != 0 {
		t.Fatalf("rejected proposal stored %d memories", len(rows))
	}

	runUser(t, r, `zemi.memory.add({ kind = "gotcha", text = "the fts join needs m-qualified columns" })`)
	runUser(t, r, `zemi.learn.decide(zemi.learn.proposals()[1].id, "accept")`)
	if rows := projectMemories(t, h); len(rows) != 1 {
		t.Fatalf("accepted proposal stored %d memories", len(rows))
	}

	runExpectErr(t, r, `zemi.learn.decide("missing", "accept")`, "no such proposal")
	runExpectErr(t, r, `zemi.learn.decide("x", "maybe")`, "accept or reject")

	err := r.ExecuteChunk(context.Background(),
		`zemi.learn.decide("whatever", "accept")`, "project.lua", lua.OriginProject)
	if err == nil || !strings.Contains(err.Error(), "user or builtin origin") {
		t.Fatalf("project-origin decide: %v", err)
	}
}
