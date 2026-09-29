package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/tools"
)

func writePermissions(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectPermissions(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	// a repository can commit .agents/permissions; only the state dir's file loads
	writePermissions(t, filepath.Join(h.Dirs.ProjectDir, ".agents", "permissions"), "curl https://example.com\n")
	writePermissions(t, h.projectPermissionsPath(), `
git push
make all

# a wrapper program is refused: no rule from a command the user did not see
python x.py
`)
	step := func() {
		if err := h.ProjectPermissionsStep().Run(context.Background(), h.RT, &lua.Generation{ID: 1}); err != nil {
			t.Fatalf("step: %v", err)
		}
	}
	step()
	if h.Perm.CheckCommandFor(perm.SubjectShell, perm.Normalize("make all"), "").Decision == perm.Allow {
		t.Error("an untrusted project must not load its permissions file")
	}
	h.SetProjectTrusted(true)
	step()
	want := []struct {
		cmd   string
		allow bool
	}{
		{"git push origin main", true},
		{"make all -j4", true},
		{"git status", false},
		{"python x.py", false},
		{"curl https://example.com", false},
	}
	for _, c := range want {
		if got := h.Perm.CheckCommandFor(perm.SubjectShell, perm.Normalize(c.cmd), "").Decision == perm.Allow; got != c.allow {
			t.Errorf("CheckCommandFor(%q) allow=%v, want %v", c.cmd, got, c.allow)
		}
	}
}

// TestProjectAskPersists: the ask's "for this project" answer writes the
// command to the permissions file and registers principal-less rules now, so
// it holds for this session and is read again on the next boot. A
// destructive command is used because auto settles a safe one in a trusted
// project, so a destructive command is the real case that still reaches the
// ask. No writePermissions call: this also proves projectAllow creates the
// dir instead of silently dropping it.
func TestProjectAskPersists(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	h.SetProjectTrusted(true)
	fakeSandbox(h, true)

	pipe := h.PipelineFor()
	ui := &fakeUI{askAllowed: true, askBy: "user:project"}
	h.SetUIHooks(ui)
	cmd := "rm -rf /tmp/scratch"
	v := pipe.Check(tools.Request{Subject: "tool.shell", Arg: cmd})
	if v.Decision != perm.Allow || v.DecidedBy != "user:project" {
		t.Fatalf("project shell: %v by %q", v.Decision, v.DecidedBy)
	}
	if len(ui.asked) != 1 || !ui.asked[0].Project || ui.asked[0].Always != "allow this exact command outside the sandbox" {
		t.Fatalf("asked: %+v", ui.asked)
	}

	b, err := os.ReadFile(h.projectPermissionsPath())
	if err != nil {
		t.Fatalf("reading the permissions file: %v", err)
	}
	if !strings.Contains(string(b), cmd) {
		t.Fatalf("permissions file missing command:\n%s", b)
	}

	// the exact rule answers the same command again without asking
	ui.asked = nil
	if got := pipe.Check(tools.Request{Subject: "tool.shell", Arg: cmd}); got.Decision != perm.Allow {
		t.Fatalf("ruled re-check: %v by %q", got.Decision, got.DecidedBy)
	}
	if len(ui.asked) != 0 {
		t.Fatalf("ruled re-check must not ask: %+v", ui.asked)
	}
}

// TestProjectChoice: only a trusted project's safe single-line shell command
// offers the project choice; anything else (untrusted, non-shell, unsafe or
// multi-line) offers none.
func TestProjectChoice(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.StateHome = t.TempDir()
	fakeSandbox(h, false)
	choice := func(req tools.Request) string {
		label, rules := h.alwaysChoice(req)
		plabel, _ := h.projectChoice(req, label, rules)
		return plabel
	}
	if plabel := choice(tools.Request{Subject: "tool.shell", Arg: "git push origin main"}); plabel != "" {
		t.Fatalf("untrusted project must not offer project choice: %q", plabel)
	}
	h.SetProjectTrusted(true)
	for _, c := range []struct {
		name string
		req  tools.Request
		want string
	}{
		{"safe shell", tools.Request{Subject: "tool.shell", Arg: "git push origin main"}, "allow git push"},
		{"wrapper", tools.Request{Subject: "tool.shell", Arg: "sh -c 'x'"}, ""},
		{"file write", tools.Request{Subject: "fs.write", Arg: "/x"}, ""},
		{"destructive", tools.Request{Subject: "tool.shell", Arg: "rm -rf /tmp/x"}, "allow this exact command"},
		{"destructive subcommand", tools.Request{Subject: "tool.shell", Arg: "git clean -n"}, "allow this exact command"},
		// a line continuation would load as two wider commands next boot
		{"multi-line", tools.Request{Subject: "tool.shell", Arg: "git \\\npush"}, ""},
	} {
		if got := choice(c.req); got != c.want {
			t.Errorf("%s: projectChoice = %q, want %q", c.name, got, c.want)
		}
	}
	// the file loads every segment of the line next boot, so a line with a
	// segment the session answer skipped (a model rule allows it already)
	// would persist a command the label never named
	h.Perm.Add(perm.Rule{Subject: "tool.shell", Pattern: "curl *", Decision: perm.Allow, Principal: "model"})
	if label, _ := h.alwaysChoice(tools.Request{Subject: "tool.shell", Arg: "curl https://x.example/a && go vet ./..."}); label != "allow go vet" {
		t.Fatalf("session label = %q", label)
	}
	if got := choice(tools.Request{Subject: "tool.shell", Arg: "curl https://x.example/a && go vet ./..."}); got != "" {
		t.Errorf("skipped segment must offer no project choice: %q", got)
	}
}
