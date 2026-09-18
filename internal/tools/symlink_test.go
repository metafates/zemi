package tools

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"

	"github.com/metafates/zemi/internal/perm"
)

func TestFileToolsRefuseSymlink(t *testing.T) {
	p, _ := testPipeline(t, false, perm.Rule{Subject: "fs.write", Pattern: "*", Decision: perm.Allow})
	all := All(p)
	dir := perm.RealPath(t.TempDir())
	target, link := filepath.Join(dir, "real.txt"), filepath.Join(dir, "link.txt")
	os.WriteFile(target, []byte("one\n"), 0o644)
	os.Symlink(target, link)
	want := link + " is a symbolic link to " + target + "; edit " + target + " instead"
	for _, c := range []struct {
		tool  fantasy.AgentTool
		input string
	}{
		{all[2], `{"path":"` + link + `","content":"x"}`},
		{all[3], `{"path":"` + link + `","old_string":"one","new_string":"x"}`},
		{all[4], `{"path":"` + link + `","edits":[{"old_string":"one","new_string":"x"}]}`},
	} {
		if resp := call(t, c.tool, c.input); !resp.IsError || resp.Content != want {
			t.Errorf("%s: %+v", c.tool.Info().Name, resp)
		}
	}
	if data, _ := os.ReadFile(target); string(data) != "one\n" {
		t.Errorf("target changed: %q", data)
	}
}
