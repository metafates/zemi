package hostapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/tools"
)

// TestAutoModeLocalApprovals: auto mode settles reads and writes inside the
// jail on its own. A path outside it, a secret behind an innocent name, and
// anything leaving the machine go on to the screening model instead.
func TestAutoModeLocalApprovals(t *testing.T) {
	h, _ := newTestHost(t)
	inside := filepath.Join(h.Dirs.ProjectDir, "main.go")
	secret := filepath.Join(h.Dirs.ProjectDir, ".env")
	link := filepath.Join(h.Dirs.ProjectDir, "notes.md")
	os.WriteFile(secret, []byte("K=v"), 0o600)
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		subject, arg string
		want         bool
	}{
		{"fs.read", inside, true},
		{"fs.write", inside, true},
		{"fs.read", secret, false},
		{"fs.read", link, false},
		{"fs.read", "/etc/passwd", false},
		{"fs.read", filepath.Join(os.Getenv("HOME"), ".ssh", "id_ed25519"), false},
		{"net.evil.tld", "https://evil.tld/?d=leak", false},
	} {
		if got := h.autoLocal(tools.Request{Subject: c.subject, Arg: c.arg}); got != c.want {
			t.Errorf("%s %s: got %v, want %v", c.subject, c.arg, got, c.want)
		}
	}
}

type promptStub struct {
	reflectStubModel
	prompt string
}

func (m *promptStub) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.prompt = fmt.Sprint(call.Prompt)
	return m.reflectStubModel.Generate(ctx, call)
}

// TestAutoApprovePreview: a write's diff shows the file's current content,
// so the screening model sees it only for files the model may read anyway.
func TestAutoApprovePreview(t *testing.T) {
	h, _ := newTestHost(t)
	m := &promptStub{reflectStubModel: reflectStubModel{resp: "SAFE"}}
	h.modelFn = func() (fantasy.LanguageModel, error) { return m, nil }
	for _, c := range []struct {
		subject, arg string
		want         bool
	}{
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, "main.go"), true},
		{"fs.write", filepath.Join(h.Dirs.ProjectDir, ".env"), false},
		{"fs.write", filepath.Join(t.TempDir(), "main.go"), false},
		{"tool.workflow", "review", true},
	} {
		h.autoApprove(tools.Request{Subject: c.subject, Arg: c.arg, Preview: "PREVIEW"})
		if got := strings.Contains(m.prompt, "PREVIEW"); got != c.want {
			t.Errorf("%s %s: preview sent %v, want %v", c.subject, c.arg, got, c.want)
		}
	}
}
