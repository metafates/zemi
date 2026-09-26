//go:build unix

package perm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestPathActsOnWhatWasChecked: a directory swapped for a symlink after the
// check fails the call instead of leading outside.
func TestPathActsOnWhatWasChecked(t *testing.T) {
	read := func(p Path) error {
		f, err := p.Open(os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		if b, _ := io.ReadAll(f); string(b) != "in" {
			return fmt.Errorf("read %q", b)
		}
		return nil
	}
	for _, c := range []struct {
		name    string
		resolve func(string) (Path, error)
		rel     string
		swap    bool // d becomes a symlink to outside after the check
		op      func(Path) error
		wantErr bool
	}{
		{"open reaches the checked file", Resolve, "d/f", false, read, false},
		{"open refuses a swapped directory", Resolve, "d/f", true, read, true},
		{"remove refuses a swapped directory", Resolve, "d/f", true, Path.Remove, true},
		{"remove takes a link, not its target", Lresolve, "link", false, Path.Remove, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, outside := RealPath(t.TempDir()), RealPath(t.TempDir())
			os.Mkdir(filepath.Join(root, "d"), 0o755)
			os.WriteFile(filepath.Join(root, "d", "f"), []byte("in"), 0o644)
			os.WriteFile(filepath.Join(outside, "f"), []byte("out"), 0o644)
			os.Symlink(filepath.Join(root, "d", "f"), filepath.Join(root, "link"))
			p, err := c.resolve(filepath.Join(root, c.rel))
			if err != nil {
				t.Fatal(err)
			}
			if c.swap {
				os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old"))
				os.Symlink(outside, filepath.Join(root, "d"))
			}
			if err := c.op(p); (err != nil) != c.wantErr {
				t.Fatalf("err = %v, want error %v", err, c.wantErr)
			}
			if b, _ := os.ReadFile(filepath.Join(outside, "f")); string(b) != "out" {
				t.Fatalf("outside file is %q", b)
			}
			if _, err := os.Stat(filepath.Join(root, "d", "f")); !c.swap && err != nil {
				t.Fatalf("checked file: %v", err)
			}
		})
	}
}
