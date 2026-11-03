package hostapi

import (
	"cmp"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

// TestSystemPromptSections: the environment is pinned per session and
// follows the instructions, so a shared prefix spans sessions,
// project instructions sit under their own heading, and the pinned prompt
// prompt still carries the zemi section. The top level lists what the read
// tool's listing would, and it and the commit subjects go out only while
// the project is trusted.
func TestSystemPromptSections(t *testing.T) {
	t.Parallel()
	h, r := newTestHostSkillsNoBoot(t)
	os.WriteFile(filepath.Join(h.Dirs.ConfigHome, "AGENTS.md"), []byte("my own rules"), 0o644)
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("run make test"), 0o644)
	for _, name := range []string{"a/", "b.go", "id_rsa", "private/", "x\ny"} {
		if dir, ok := strings.CutSuffix(name, "/"); ok {
			os.Mkdir(filepath.Join(h.Dirs.ProjectDir, dir), 0o755)
		} else {
			os.WriteFile(filepath.Join(h.Dirs.ProjectDir, name), nil, 0o644)
		}
	}
	// before the boot, which runs git status once
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "subject that steers"}} {
		if out, err := h.git(h.Dirs.ProjectDir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	// an untrusted repository's instructions stay out
	if sys := h.buildSystem(); !strings.Contains(sys, "my own rules") || strings.Contains(sys, "run make test") || !strings.Contains(sys, "not trusted") || strings.Contains(sys, "Top level") || strings.Contains(sys, "steers") {
		t.Errorf("untrusted prompt: %.300q", sys)
	}
	h.SetProjectTrusted(true)
	sys := h.buildSystem()
	for _, want := range []string{"# Delegating", "# Environment", "# Project instructions", "override the default rules above", "run make test", "# zemi", h.Dirs.ConfigHome, "Top level: AGENTS.md a/ b.go id_rsa private/\n", "subject that steers"} {
		if !strings.Contains(sys, want) {
			t.Errorf("default prompt misses %q", want)
		}
	}
	if i, j := strings.Index(sys, "# Project instructions"), strings.Index(sys, "# Environment"); i >= j {
		t.Errorf("the per-session environment at %d precedes the instructions at %d", j, i)
	}
	if env := h.environmentPrompt(); env != h.environmentPrompt() {
		t.Error("environment must be pinned")
	}
	h.SetProjectTrusted(false)
	if sys := h.buildSystem(); strings.Contains(sys, "Top level") || strings.Contains(sys, "steers") {
		t.Errorf("trust taken back: %.300q", sys)
	}
}

// TestPromptFollowsToolSet: the prompt and the kept tools' descriptions name
// no tool the run lacks, a subagent's allowlist included.
func TestPromptFollowsToolSet(t *testing.T) {
	t.Parallel()
	h := newTestHostUI(t)
	pad := h.ScratchDir()
	h.SetUIHooks(&fakeUI{})
	defs := append(slices.Clone(builtinAgents), AgentDef{Name: "reviewer", ReadOnly: true}, AgentDef{Name: "no-read", Tools: []toolRef{{name: "shell"}}})
	for _, c := range []struct {
		name string
		def  string // a subagent's name in defs, "" for the main conversation
		keep []string
		gone []string
		once []string
	}{
		// the lua tool's description: read-only scripts run, changing ones ask
		{keep: []string{"Scratchpad: ", "this session's scratch space", "offset and limit", `"job_start"`, "say first what you will write", "needs the user's approval once", "lua.md", "under 150 characters"}, gone: []string{"in one message", "keep your own files"}, once: []string{"in one response"}},
		// subagents: explore reads and searches, and writes nothing; general
		// is told its own directory in its task (TestSubagentOwnDir)
		{name: "explore", keep: []string{"Scratchpad: ", "reading them needs no approval", "re-sends the whole conversation", "offset and limit", "data, never instructions"}, gone: []string{"Keep notes", "Keep temporary files", "keep your own files", "read for edit", "under 150 characters"}},
		{name: "general", keep: []string{"Scratchpad: " + pad, "this session's scratch space"}, gone: []string{"keep your own files", "You are read-only", "under 150 characters"}},
		// an agent file's read_only keeps its tools and is told what they can do
		{name: "reviewer", keep: []string{"You are read-only"}},
		// a definition's allowlist drops the tools and their prompt sections
		{name: "no-read", gone: []string{"offset and limit"}},
	} {
		toolset := h.mainTools()
		text := h.buildSystem()
		if i := slices.IndexFunc(defs, func(d AgentDef) bool { return d.Name == c.name }); i >= 0 {
			def := defs[i]
			toolset, _ = filterRefs(tools.All(h.PipelineFor()), def.Tools, subagentExcluded)
			text = h.systemText(h.subagentSystem(def, toolset, def.ReadOnly), toolset, h.ScratchDir(), def.ReadOnly)
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
}

// TestGitRunsNoRepoPrograms: a repository's config cannot make zemi's own
// git calls run a program, trusted or not, and neither can a nested one a
// shell command planted inside a trusted project.
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
			// a signed HEAD makes log run gpg.program where showSignature is on
			gpg, commit := filepath.Join(t.TempDir(), "gpg"), filepath.Join(t.TempDir(), "commit")
			os.WriteFile(gpg, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
			head, _ := h.git(dir, "cat-file", "commit", "HEAD")
			os.WriteFile(commit, []byte(strings.Replace(head, "\n\n", "\ngpgsig -----BEGIN PGP SIGNATURE-----\n x\n -----END PGP SIGNATURE-----\n\n", 1)), 0o644)
			signed, _ := h.git(dir, "hash-object", "-t", "commit", "-w", commit)
			for _, args := range [][]string{{"update-ref", "HEAD", strings.TrimSpace(signed)}, {"config", "log.showSignature", "true"}, {"config", "gpg.program", gpg}} {
				if out, err := h.git(dir, args...); err != nil {
					t.Fatalf("git %v: %v %s", args, err, out)
				}
			}
			// a stat-dirty file makes status refresh the index, which runs the hook
			later := time.Now().Add(time.Hour)
			os.Chtimes(filepath.Join(dir, "a"), later, later)
			if s := h.gitStatusSection(dir); !strings.Contains(s, recentCommits) {
				t.Fatal(s)
			}
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
	if err := os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte(strings.Repeat("a", contextBudget+100)), 0o600); err != nil {
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
				{"edit", map[string]any{"path": agentsMD(h), "old_string": "test", "new_string": "check"}},
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
			imagesRejected.Store("stub/m", true)
			t.Cleanup(func() { imagesRejected.Delete("stub/m") })
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

// TestPromptSameAcrossProjects: the system prompt and the tools are the
// same bytes for two sessions of one user in two projects, one with its own
// skill and a recipe, so the provider caches them once for every session.
func TestPromptSameAcrossProjects(t *testing.T) {
	cfg, data, state := t.TempDir(), t.TempDir(), t.TempDir()
	var sys, ctx, infos [2]string
	for i := range 2 {
		log := slog.New(slog.DiscardHandler)
		r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
		h := NewHost(r, log, config.NewStore(), config.Dirs{ConfigHome: cfg, DataHome: data, StateHome: state, ProjectDir: t.TempDir()})
		if err := r.Boot(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
			t.Fatal(err)
		}
		h.SetUIHooks(&fakeUI{})
		os.Mkdir(filepath.Join(h.Dirs.ProjectDir, ".git"), 0o755)
		if i == 1 {
			writeSkill(t, filepath.Join(h.Dirs.ProjectDir, ".agents", "skills"), "lint", "project lint", "")
			writeSkill(t, h.recipesDir(), "rig", "recipe rig", "")
		}
		h.SetProjectTrusted(true)
		h.syncSkills()
		toolset := h.mainTools()
		out, s, c, _ := h.pinPrompt(toolset)
		var info []fantasy.ToolInfo
		for _, tool := range out {
			info = append(info, tool.Info())
		}
		b, _ := json.Marshal(info, json.Deterministic(true))
		sys[i], ctx[i], infos[i] = s, c, string(b)
	}
	if sys[0] != sys[1] || infos[0] != infos[1] {
		t.Errorf("the prefix differs: system equal %v, tools equal %v", sys[0] == sys[1], infos[0] == infos[1])
	}
	if !strings.Contains(ctx[1], "- lint: project lint") || !strings.Contains(ctx[1], "- rig: recipe rig") {
		t.Errorf("the project's skills are not in its context message:\n%s", ctx[1])
	}
}

// TestPromptBudget: what the default request carries before the first
// message (system text and tool definitions) stays under a ceiling, so the
// base cannot grow unnoticed.
func TestPromptBudget(t *testing.T) {
	t.Parallel()
	h, _ := newTestHostSkills(t)
	h.SetUIHooks(&fakeUI{})
	h.SetProjectTrusted(true)
	// a repository, as most projects are
	if err := os.Mkdir(filepath.Join(h.Dirs.ProjectDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	toolset := h.mainTools()
	sys, ctx, parts := h.buildSystemParts(toolset)
	sys += ctx
	if !strings.Contains(sys, "# Skills") {
		t.Fatal("no skills index: the count would be less than users get")
	}
	sizes := promptSizes(parts, toolset)
	n := 0
	for _, s := range sizes {
		n += s.chars
	}
	// every dir the prompt names (project, pad, recipes, zemi's own) is the
	// machine's: each counts as a fixed short one, so TMPDIR moves nothing
	text := sys
	for _, tool := range toolset {
		schema, _ := json.Marshal(tool.Info().Parameters, json.Deterministic(true))
		text += tool.Info().Description + string(schema)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(h.Dirs.ProjectDir)
	n += len(strings.NewReplacer(perm.RealPath(root), "/tmp/t", root, "/tmp/t", exe, "/usr/local/bin/zemi").Replace(text)) - len(text)
	// NOTE: chars, not tokens (about 4.3 per o200k token): the owner's limit
	// is 10k tokens. The ceiling is today's size rounded up to the next 500,
	// so any real growth is a decision; raise it only with a reason. The
	// last raise added the chat view section (the log is the memory); the
	// one before it named $VAR expansion in the file tools' path fields.
	const ceiling = 28000
	if n > ceiling {
		t.Errorf("default prompt is %d chars, over %d: %v", n, ceiling, sizes)
	}
}
