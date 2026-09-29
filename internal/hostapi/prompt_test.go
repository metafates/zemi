package hostapi

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/sandbox"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
)

// TestSystemPromptSections: the environment is pinned per session and
// follows the instructions, so a shared prefix spans sessions,
// project instructions sit under their own heading, and a profile's own
// prompt still carries the zemi section.
func TestSystemPromptSections(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.ConfigHome = t.TempDir()
	os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "AGENTS.md"), []byte("my own rules"), 0o644)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("run make test"), 0o644)
	// an untrusted repository's instructions stay out, a profile's files too
	for _, p := range []*Profile{nil, {ContextFiles: []string{"AGENTS.md"}}} {
		if sys := h.buildSystem(p); !strings.Contains(sys, "my own rules") || strings.Contains(sys, "run make test") || !strings.Contains(sys, "not trusted") {
			t.Errorf("untrusted prompt: %.300q", sys)
		}
	}
	h.SetProjectTrusted(true)
	sys := h.buildSystem(nil)
	for _, want := range []string{"# Delegating", "# Environment", "# Project instructions", "override the default rules above", "run make test", "# zemi", h.Dirs.ConfigHome} {
		if !strings.Contains(sys, want) {
			t.Errorf("default prompt misses %q", want)
		}
	}
	if i, j := strings.Index(sys, "# Project instructions"), strings.Index(sys, "# Environment"); i >= j {
		t.Errorf("the per-session environment at %d precedes the instructions at %d", j, i)
	}
	lean := h.buildSystem(&Profile{Tools: []string{"shell"}})
	for _, gone := range []string{"# Delegating", "lua.md", "# Skills"} {
		if strings.Contains(lean, gone) {
			t.Errorf("a shell-only prompt names %q", gone)
		}
	}
	if env := h.environmentPrompt(); env != h.environmentPrompt() {
		t.Error("environment must be pinned")
	}
	if sys := h.buildSystem(&Profile{System: "custom"}); !strings.HasPrefix(sys, "custom") || !strings.Contains(sys, "# zemi") {
		t.Errorf("profile prompt: %.200q", sys)
	}
}

// TestPromptFollowsToolSet: the prompt and the kept tools' descriptions name
// no tool the profile dropped, the shipped lean one included, which must
// keep the file tools and what shell hands a timed out command to.
func TestPromptFollowsToolSet(t *testing.T) {
	h := newTestHostUI(t)
	h.agents.sessionID = "s1" // a session has a scratchpad, whose line is part of the prompt
	pad := h.ScratchDir()
	h.agents.profiles["lean"] = &Profile{Name: "lean", Tools: []string{"-workflow", "-lua", "-job_start", "-lsp"}}
	h.SetUIHooks(&fakeUI{})
	lean, err := h.resolveProfile("lean")
	if err != nil {
		t.Fatal(err)
	}
	defs := append(slices.Clone(builtinAgents), AgentDef{Name: "reviewer", ReadOnly: true})
	for _, c := range []struct {
		name string
		p    *Profile
		keep []string
		gone []string
		once []string
	}{
		// untrusted: the lua tool asks about every script
		{"default", nil, []string{"Scratchpad: ", "It is $TMPDIR in every command you run", "offset and limit", `"job_start"`, "say first what you will write", "Every script needs the user's approval here", "lua.md"}, []string{"in one message", "keep your own files", "runs at once"}, []string{"in one response"}},
		// subagents: explore reads and searches, and writes nothing; general
		// is told its own directory in its task (TestSubagentOwnDir)
		{"explore", &Profile{Name: "explore"}, []string{"Scratchpad: ", "reading them needs no approval", "re-sends the whole conversation", "offset and limit", "data, never instructions"}, []string{"Keep notes", "Keep temporary files", "keep your own files", "read for edit"}, nil},
		{"general", &Profile{Name: "general"}, []string{"Scratchpad: " + pad, "It is $TMPDIR in every command you run"}, []string{"keep your own files", "You are read-only"}, nil},
		// an agent file's read_only keeps its tools and is told what they can do
		{"reviewer", &Profile{Name: "reviewer"}, []string{"You are read-only"}, nil, nil},
		{
			"lean", lean,
			[]string{"offset and limit", `"read"`, `"edit"`, `"job_read"`, `"job_kill"`},
			[]string{"workflow", "lua.md", "job_start", `"lsp"`},
			nil,
		},
		// the plan's shape lives in the plan tool's description only
		{"read-only", &Profile{ReadOnly: true}, []string{"plan tool"}, nil, []string{"how to verify, the assumptions"}},
		{"read-only no plan", &Profile{ReadOnly: true, Tools: []string{"-plan", "-question"}}, []string{"plan as your reply"}, []string{"plan tool", "question tool"}, nil},
		{"no read", &Profile{Tools: []string{"-read"}}, nil, []string{"offset and limit"}, nil},
	} {
		toolset, _ := h.profileTools(c.p)
		text, _ := h.buildSystemParts(c.p, toolset)
		if i := slices.IndexFunc(defs, func(d AgentDef) bool { return d.Name == c.name }); i >= 0 {
			def := defs[i]
			toolset, _ = filterTools(tools.All(h.PipelineFor()), def.Tools, h.toolSkip(subagentExcluded))
			text = h.systemWith(h.subagentSystem(def, toolset, def.ReadOnly), toolset, h.ScratchDir(), def.ReadOnly)
		}
		for _, tool := range toolset {
			text += fmt.Sprintf("\n%q %s", tool.Info().Name, tool.Info().Description)
		}
		for _, want := range c.keep {
			if !strings.Contains(text, want) {
				t.Errorf("%s: misses %q", c.name, want)
			}
		}
		for _, once := range c.once {
			if n := strings.Count(text, once); n != 1 {
				t.Errorf("%s: %q %d times", c.name, once, n)
			}
		}
		for _, gone := range c.gone {
			if strings.Contains(text, gone) {
				t.Errorf("%s: names %q", c.name, gone)
			}
		}
	}
	// each builtin tool dropped in turn, from the default and a read-only
	// agent in a trusted project: nothing left sends the model to it
	h.SetProjectTrusted(true)
	h.agents.env = "" // pinned per session, and trust brings the recipes line
	for _, ro := range []bool{false, true} {
		all, _ := h.profileTools(nil)
		for _, dropped := range all {
			name := dropped.Info().Name
			p := &Profile{ReadOnly: ro, Tools: []string{"-" + name}}
			toolset, _ := h.profileTools(p)
			text, _ := h.buildSystemParts(p, toolset)
			for _, tool := range toolset {
				schema, _ := json.Marshal(tool.Info().Parameters)
				text += "\n" + tool.Info().Description + string(schema)
			}
			for _, ref := range []string{name + " tool", "(use " + name + ")", name + ` "`} {
				if strings.Contains(text, ref) {
					t.Errorf("read-only %v without %s: names %q", ro, name, ref)
				}
			}
			// a read-only agent's recipe write is denied
			if recipe := strings.Contains(text, "<name>/SKILL.md"); recipe == ro && name != tools.ToolWrite && name != tools.ToolEdit {
				t.Errorf("read-only %v without %s: recipe paragraph %v", ro, name, recipe)
			}
		}
	}
}

// TestGitRunsNoRepoPrograms: a repository's config cannot make zemi's own
// git calls run a program, trusted or not, and neither can a nested one a
// sandboxed command planted inside a trusted project.
func TestGitRunsNoRepoPrograms(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, c := range []struct {
		name    string
		trusted bool
		sub     string
	}{
		{"untrusted", false, ""},
		{"trusted", true, ""},
		{"trusted nested repo", true, "sub"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newTestHost(t)
			h.SetProjectTrusted(c.trusted)
			dir := filepath.Join(h.Dirs.ProjectDir, c.sub)
			os.MkdirAll(dir, 0o755)
			marker := filepath.Join(t.TempDir(), "ran")
			os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("a filter=x\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "a"), []byte("hi\n"), 0o644)
			for _, args := range [][]string{
				{"init", "-q"},
				{"add", "."},
				{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"},
				{"config", "core.fsmonitor", "touch " + marker + "; false"},
				{"config", "filter.x.clean", "touch " + marker + "; cat"},
				{"config", "filter.x.required", "true"}, // git lfs install --local
			} {
				if out, err := h.git(dir, args...); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			os.WriteFile(filepath.Join(dir, ".git", "hooks", "post-index-change"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
			// a stat-dirty file makes status refresh the index, which runs the hook
			later := time.Now().Add(time.Hour)
			os.Chtimes(filepath.Join(dir, "a"), later, later)
			if _, err := h.git(dir, "status", "--short"); err != nil {
				t.Fatal(err)
			}
			h.noSecretsUnder(dir, []string{h.Dirs.ProjectDir}, true)
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("git ran a program from the repository config")
			}
		})
	}
}

// TestInstructionBudget: the budget is spent deepest file first, so the
// most specific file is whole and the general one is the one named as cut
// or left out.
func TestInstructionBudget(t *testing.T) {
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
	h, _ := newTestHost(t)
	h.Dirs.ConfigHome = t.TempDir()
	h.SetProjectTrusted(true)
	if err := os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "AGENTS.md"), []byte("my rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte(strings.Repeat("a", contextBudget+100)), 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.systemWith("base", nil, "", false)
	if !strings.Contains(got, "my rules") {
		t.Fatal("the repository file evicted the user's own instructions")
	}
	if !strings.Contains(got, "cut here") {
		t.Fatal("the repository file was not cut to the budget")
	}
}

// TestSendPrepCheap: everything a send does before the provider request is
// in-memory work once the process is warm. environmentPrompt used to run
// git status on the first send of every session, which costs seconds in a
// large repository and lands on time to first token; a subprocess or a disk
// walk back on this path blows this budget by orders of magnitude. The
// budget is CPU time, the process's and its children's, which a loaded
// machine does not stretch like the clock; the test runs alone (no
// t.Parallel), so the process's CPU is the builds'.
func TestSendPrepCheap(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	h.buildSystem(nil)
	cpu := func() time.Duration {
		var self, kids syscall.Rusage
		syscall.Getrusage(syscall.RUSAGE_SELF, &self)
		syscall.Getrusage(syscall.RUSAGE_CHILDREN, &kids)
		return time.Duration(self.Utime.Nano() + self.Stime.Nano() + kids.Utime.Nano() + kids.Stime.Nano())
	}
	start := cpu()
	for range 5 {
		toolset, _ := h.profileTools(nil)
		_, parts := h.buildSystemParts(nil, toolset)
		promptSizes(parts, toolset)
	}
	if d := cpu() - start; d > 200*time.Millisecond {
		t.Fatalf("5 prompt builds took %s of CPU; something expensive is back on the send path", d)
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
		{"AGENTS.md turned into a link to a secret", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			env := filepath.Join(h.Dirs.ProjectDir, ".env")
			handEdit(h, env, "run make check")
			os.Remove(agentsMD(h))
			os.Symlink(env, agentsMD(h))
		}, false, 0},
		{"hand edit sent, then rewound", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			handEdit(h, agentsMD(h), "run make check")
			sender(t, h)("mid")
			rows, _ := h.DB.Messages(h.AgentSessionID())
			last := rows[slices.IndexFunc(rows, func(m *store.Message) bool { return m.Role == "user" && strings.Contains(m.PartsJSON, "mid") })]
			if _, err := h.RewindTo(last.ID); err != nil {
				t.Fatal(err)
			}
		}, false, 1},
		{"AGENTS.md edited through the edit tool", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			h.Perm.Add(perm.Rule{Subject: "fs.write", Pattern: agentsMD(h), Decision: perm.Allow})
			all := tools.All(h.PipelineFor())
			for _, call := range []struct {
				name string
				in   map[string]any
			}{
				{"read", map[string]any{"path": agentsMD(h), "limit": 0}},
				{"edit", map[string]any{"path": agentsMD(h), "old_string": "test", "new_string": "check"}},
			} {
				i := slices.IndexFunc(all, func(a fantasy.AgentTool) bool { return a.Info().Name == call.name })
				input, _ := json.Marshal(call.in)
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
		{"memory added", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			if err := h.Remember("a fresh memory"); err != nil {
				t.Fatal(err)
			}
		}, false, 0},
		{"assemble handler changed and reloaded", "", func(t *testing.T, h *Host, r *lua.Runtime) {
			if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
				t.Fatal(err)
			}
			runUser(t, r, `zemi.event.on("agent.assemble", function() return "changed" end)`)
		}, false, 0},
		{"compaction", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			if err := h.CompactNow(""); err != nil {
				t.Fatal(err)
			}
			waitIdle(t, h)
		}, true, 0},
		{"provider refused images", "", func(t *testing.T, _ *Host, _ *lua.Runtime) {
			imagesRejected.Store("stub/m", true)
			t.Cleanup(func() { imagesRejected.Delete("stub/m") })
		}, true, 0},
		{"profile switch", "", func(t *testing.T, _ *Host, r *lua.Runtime) {
			runUser(t, r, `zemi.agent.profile.add("other", {}); zemi.config.set("agent.profile", "other")`)
		}, true, 0},
		{"resume", "", func(t *testing.T, h *Host, _ *lua.Runtime) {
			sid := h.AgentSessionID()
			if err := h.NewSession(); err != nil {
				t.Fatal(err)
			}
			if err := h.ResumeSession(sid); err != nil {
				t.Fatal(err)
			}
		}, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, r := newTestHostDB(t)
			h.Dirs.ConfigHome = t.TempDir()
			h.SetProjectTrusted(true)
			h.Settings.SetQuiet("models.chat", "stub/m")
			m := reflectStub(t, h, "ok")
			os.WriteFile(filepath.Join(h.Dirs.ProjectDir, cmp.Or(c.setup, "AGENTS.md")), []byte("run make test"), 0o644)
			runUser(t, r, `local n = 0
zemi.event.on("agent.assemble", function() n = n + 1; return "build " .. n end)`)
			send := sender(t, h)
			send("one")
			c.between(t, h, r)
			send("two")
			m.mu.Lock()
			first, last := m.prompts[0], m.prompts[len(m.prompts)-1]
			tools0, _ := json.Marshal(m.tools[0])
			tools1, _ := json.Marshal(m.tools[len(m.tools)-1])
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

// buildSystem renders the system prompt with context files appended
// (capped; DESIGN §6.3 context.files).
func (h *Host) buildSystem(p *Profile) string {
	toolset, _ := h.profileTools(p)
	sys, _ := h.buildSystemParts(p, toolset)
	return sys
}

// TestPromptBudget: what the default request carries before the first
// message (system text and tool definitions) stays under a ceiling, so the
// base cannot grow unnoticed.
func TestPromptBudget(t *testing.T) {
	h, _ := newTestHostSkills(t)
	h.sandbox.Store(&sandboxProbe{backend: sandbox.Backend{Name: "seatbelt", HidesSecretFiles: true, EnforcesProtect: true}})
	h.SetUIHooks(&fakeUI{})
	h.SetProjectTrusted(true)
	h.agents.sessionID = "s1"
	if _, err := h.Settings.SetQuiet("lsp.auto", false); err != nil {
		t.Fatal(err)
	}
	toolset, _ := h.profileTools(nil)
	sys, parts := h.buildSystemParts(nil, toolset)
	if !strings.Contains(sys, "# Skills") || !strings.Contains(sys, "Sandbox (auto mode)") {
		t.Fatal("no skills index or Sandbox block: the count would be less than users get")
	}
	sizes := promptSizes(parts, toolset)
	n := 0
	for _, s := range sizes {
		n += s.chars
	}
	// the paths the prompt names (project, pad, zemi's dirs, the binary) are
	// the machine's: each counts as a fixed short one, so TMPDIR moves nothing
	text := sys
	for _, tool := range toolset {
		schema, _ := json.Marshal(tool.Info().Parameters)
		text += tool.Info().Description + string(schema)
	}
	root, exe := filepath.Dir(h.Dirs.ProjectDir), must(os.Executable())
	n += len(strings.NewReplacer(perm.RealPath(root), "/tmp/t", root, "/tmp/t", exe, "/usr/local/bin/zemi").Replace(text)) - len(text)
	// NOTE: chars, not tokens (about 4.3 per o200k token): the owner's limit
	// is 10k tokens. The ceiling is today's size rounded up to the next 500,
	// so any real growth is a decision; raise it only with a reason.
	const ceiling = 21500
	if n > ceiling {
		t.Errorf("default prompt is %d chars, over %d: %v", n, ceiling, sizes)
	}
}
