package hostapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	for _, want := range []string{"# Delegating", "# Environment", "# Project instructions", "run make test", "# zemi", h.Dirs.ConfigHome} {
		if !strings.Contains(sys, want) {
			t.Errorf("default prompt misses %q", want)
		}
	}
	if env := h.environmentPrompt(); env != h.environmentPrompt() {
		t.Error("environment must be pinned")
	}
	if sys := h.buildSystem(&Profile{System: "custom"}); !strings.HasPrefix(sys, "custom") || !strings.Contains(sys, "# zemi") {
		t.Errorf("profile prompt: %.200q", sys)
	}
}

// TestGitRunsNoRepoPrograms: a repository's config cannot make zemi's own
// git calls run a program, trusted or not, and neither can a nested one a
// sandboxed command planted inside a trusted project.
func TestGitRunsNoRepoPrograms(t *testing.T) {
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
	got := h.systemWith("base", nil)
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
	h.buildSystemParts(nil)
	h.profileTools(nil)
	start := time.Now()
	for range 5 {
		_, parts := h.buildSystemParts(nil)
		promptSizes(parts, h.profileTools(nil))
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("5 prompt builds took %s; something expensive is back on the send path", d)
	}
}
