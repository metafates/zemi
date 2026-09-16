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
	os.WriteFile(filepath.Join(h.Dirs.ProjectDir, "AGENTS.md"), []byte("run make test"), 0o644)
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

// TestGitRunsNoRepoPrograms: a cloned or unpacked project's .git/config
// cannot make zemi's own git calls run a program.
func TestGitRunsNoRepoPrograms(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h, _ := newTestHost(t)
	dir := h.Dirs.ProjectDir
	marker := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("a filter=x\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a"), []byte("hi\n"), 0o644)
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"},
		{"config", "core.fsmonitor", "touch " + marker + "; false"},
		{"config", "filter.x.clean", "touch " + marker + "; cat"},
	} {
		if out, err := h.runGit(dir, args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, ".git", "hooks", "post-index-change"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	// a stat-dirty file makes status refresh the index, which runs the hook
	later := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(dir, "a"), later, later)
	if !strings.Contains(h.environmentPrompt(), "Git status") {
		t.Fatal("status failed")
	}
	h.noSecretsUnder(dir, []string{dir}, true)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("git ran a program from the repository config")
	}
}
