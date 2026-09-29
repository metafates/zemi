package hostapi

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/store"
)

// TestListProjects: every root with a checked root file or a trust decision
// is listed, the last opened first, with its trust word and session count.
func TestListProjects(t *testing.T) {
	home, _ := os.UserHomeDir()
	tmp := t.TempDir()
	dirs := config.Dirs{StateHome: t.TempDir()}
	for _, c := range []struct {
		root     string
		trust    string
		opened   time.Duration // ago; 0 writes no root file
		sessions int
	}{
		{root: filepath.Join(tmp, "trusted"), trust: "trusted", opened: time.Minute, sessions: 2},
		{root: filepath.Join(tmp, "undecided"), opened: time.Hour},
		{root: filepath.Join(tmp, "decision-only"), trust: "untrusted"},
		{root: filepath.Join(tmp, "neither"), sessions: 1},
		{root: "/zemi-test-missing/x", trust: "trusted", opened: 2 * time.Hour},
		{root: home, opened: 3 * time.Hour},
	} {
		_ = os.MkdirAll(c.root, 0o755) // the missing one's parent is not writable
		if c.opened != 0 || c.sessions > 0 {
			g, p, err := store.OpenForDirs(dirs.StateHome, c.root)
			if err != nil {
				t.Fatal(err)
			}
			for i := range c.sessions {
				if err := p.CreateSession(strconv.Itoa(i), "", "", ""); err != nil {
					t.Fatal(err)
				}
			}
			g.Close()
			p.Close()
			rootFile := filepath.Join(filepath.Dir(store.ProjectDBPath(dirs.StateHome, c.root)), "root")
			if c.opened == 0 {
				_ = os.Remove(rootFile)
			} else if at := time.Now().Add(-c.opened); os.Chtimes(rootFile, at, at) != nil {
				t.Fatal("no root file")
			}
		}
		if c.trust != "" {
			saved := dirs.ReadTrust()
			saved[c.root] = c.trust
			if err := dirs.WriteTrust(saved); err != nil {
				t.Fatal(err)
			}
		}
	}
	var got []string
	for _, p := range ListProjects(dirs, true) {
		got = append(got, fmt.Sprintf("%s %s %d", p.Root, p.TrustWord(), p.Sessions))
	}
	want := []string{
		filepath.Join(tmp, "trusted") + " trusted 2",
		filepath.Join(tmp, "undecided") + " undecided 0",
		"/zemi-test-missing/x missing 0",
		home + " untrusted 0",
		filepath.Join(tmp, "decision-only") + " untrusted 0",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ListProjects:\n%q\nwant\n%q", got, want)
	}
}
