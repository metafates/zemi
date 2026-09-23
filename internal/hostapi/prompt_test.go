package hostapi

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/tools"
)

// TestSystemPromptSections: the environment is pinned per session, project
// instructions sit under their own heading, and a profile's own prompt
// still carries the zemi section.
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
	for _, want := range []string{"# Delegating", "# Environment", "# Project instructions", "override the system's default rules", "run make test", "# zemi", h.Dirs.ConfigHome} {
		if !strings.Contains(sys, want) {
			t.Errorf("default prompt misses %q", want)
		}
	}
	lean := h.buildSystem(&Profile{Tools: []string{"bash"}})
	for _, gone := range []string{"# Delegating", "zemi-customize", "Lua tool", "# Skills"} {
		if strings.Contains(lean, gone) {
			t.Errorf("a bash-only prompt names %q", gone)
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
// keep the file tools and what bash hands a timed out command to.
func TestPromptFollowsToolSet(t *testing.T) {
	h := newTestHostUI(t)
	h.agents.sessionID = "s1" // a session has a scratchpad, whose line is part of the prompt
	h.agents.profiles["lean"] = &Profile{Name: "lean", Tools: []string{"-workflow", "-lua", "-job_start", "-lsp"}}
	h.SetUIHooks(&fakeUI{})
	lean, err := h.resolveProfile("lean")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		p    *Profile
		keep []string
		gone []string
		once []string
	}{
		{"default", nil, []string{"Scratchpad: ", "redirect it", "offset and limit", "Use todos", `"job_start"`, "zemi-config before changing its settings"}, []string{"in one message"}, []string{"in one response"}},
		// a subagent: explore reads and searches, and writes nothing
		{"explore", &Profile{Name: "explore"}, []string{"Scratchpad: ", "re-sends the whole conversation", "offset and limit"}, []string{"redirect it", "Put notes", "Use todos"}, nil},
		{"lean", lean, []string{"offset and limit", `"read"`, `"edit"`, `"job_read"`, `"job_kill"`},
			[]string{"workflow", "Lua tool", "job_start", `"lsp"`}, nil},
		// the plan's shape lives in the plan tool's description only
		{"read-only", &Profile{ReadOnly: true}, []string{"plan tool"}, nil, []string{"how to verify, the assumptions"}},
		{"read-only no plan", &Profile{ReadOnly: true, Tools: []string{"-plan", "-question"}}, []string{"plan as your reply"}, []string{"plan tool", "question tool with"}, nil},
		{"no read", &Profile{Tools: []string{"-read", "-todos"}}, nil, []string{"offset and limit", "Use todos"}, nil},
	} {
		toolset := h.profileTools(c.p)
		text, _ := h.buildSystemParts(c.p, toolset)
		if c.name == "explore" {
			toolset, _ = filterTools(tools.All(h.PipelineFor()), builtinAgents[0].Tools, h.toolSkip(subagentExcluded))
			text = h.systemWith(h.subagentSystem(builtinAgents[0], toolset), nil, toolset)
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
				{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"},
				{"config", "core.fsmonitor", "touch " + marker + "; false"},
				{"config", "filter.x.clean", "touch " + marker + "; cat"},
				{"config", "filter.x.required", "true"}, // git lfs install --local
			} {
				if out, err := h.runGit(dir, nil, args...); err != nil {
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
	got := h.systemWith("base", nil, nil)
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
// walk back on this path blows this budget by orders of magnitude.
func TestSendPrepCheap(t *testing.T) {
	h, _ := newTestHost(t)
	h.SetProjectTrusted(true)
	h.buildSystem(nil)
	start := time.Now()
	for range 5 {
		toolset := h.profileTools(nil)
		_, parts := h.buildSystemParts(nil, toolset)
		promptSizes(parts, toolset)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("5 prompt builds took %s; something expensive is back on the send path", d)
	}
}
