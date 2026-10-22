package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/proc"
)

// TestAutoUpdate walks the daily sweep: it runs when marketplaces.json's
// last one is a day old, pulls with auto on, only names the moved with auto
// off, and dates only a run that finished.
func TestAutoUpdate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	global := filepath.Join(tmp, "gitconfig")
	os.WriteFile(global, []byte("[user]\nname = t\nemail = t@t\n"), 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	d := config.Dirs{DataHome: filepath.Join(tmp, "data")}
	gitIn := func(dir string, args ...string) {
		t.Helper()
		if out, err := (proc.Git{Dir: dir, Foreign: true}).Run(t.Context(), args...); err != nil {
			t.Fatal(err, out)
		}
	}
	mkt := filepath.Join(tmp, "src", "mkt")
	version := "1.0.0"
	write(t, mkt, map[string]string{
		".zemi/marketplace.json":            `{"name": "sweep", "plugins": [{"name": "one", "source": "./plugins/one", "version": "1.0.0"}]}`,
		"plugins/one/.zemi/init.lua":        "-- one\n",
		"plugins/one/.zemi/plugin.json":     `{"version": "1.0.0"}`,
		".claude-plugin/marketplace.json":   `{"name": "claude-shadow", "plugins": []}`,
		"plugins/one/commands/hi.md":        "hi",
		"plugins/one/commands/change-me.md": version,
	})
	// a command file the sweeps bump, so a pull has something to move
	gitIn(mkt, "init", "-q")
	gitIn(mkt, "add", ".")
	gitIn(mkt, "commit", "-qm", "init")

	if err := Run(t.Context(), d, []string{"marketplace", "add", mkt}, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), d, []string{"install", "one@sweep"}, os.Stdout); err != nil {
		t.Fatal(err)
	}

	// an upstream move the clone has not seen: the sweep pulls, the linked
	// install moves with it, and the news names both
	bump := func(v string) {
		version = v
		write(t, mkt, map[string]string{"plugins/one/commands/change-me.md": v})
		write(t, mkt, map[string]string{".zemi/marketplace.json": `{"name": "sweep", "plugins": [{"name": "one", "source": "./plugins/one", "version": "` + v + `"}]}`})
		gitIn(mkt, "add", ".")
		gitIn(mkt, "commit", "-qm", v)
	}
	dated := func() {
		f, err := readMarketplaces(d)
		if err != nil {
			t.Fatal(err)
		}
		f.Checked = time.Now().UTC().Add(-25 * time.Hour)
		if err := f.save(d); err != nil {
			t.Fatal(err)
		}
	}
	installed := func(t *testing.T) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(d.PluginsRoot(), "installed", "one", "commands", "change-me.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	bump("1.1.0")
	news, _, err := AutoUpdate(t.Context(), d, true)
	if err != nil || !slices.Contains(news, "marketplace sweep updated") || !slices.Contains(news, "one 1.0.0 -> 1.1.0") {
		t.Fatalf("auto: news %v, %v", news, err)
	}
	if got := installed(t); got != "1.1.0" {
		t.Errorf("the install shows %q", got)
	}
	if news, _, _ := AutoUpdate(t.Context(), d, true); len(news) != 0 {
		t.Errorf("a fresh sweep ran again: %v", news)
	}

	// with auto off, a due sweep only fetches and names the marketplace
	bump("1.2.0")
	dated()
	news, _, err = AutoUpdate(t.Context(), d, false)
	if err != nil || !slices.Contains(news, "marketplace sweep has updates") {
		t.Fatalf("off: news %v, %v", news, err)
	}
	if got := installed(t); got != "1.1.0" {
		t.Errorf("the fetch pulled: the install shows %q", got)
	}
	if news, _, _ := AutoUpdate(t.Context(), d, false); len(news) != 0 {
		t.Errorf("the fetch did not date the sweep: %v", news)
	}

	// the user's pull moves the install
	if err := Run(t.Context(), d, []string{"marketplace", "update", "sweep"}, os.Stdout); err != nil {
		t.Fatal(err)
	}
	if got := installed(t); got != "1.2.0" {
		t.Errorf("the pull did not move the install: %q", got)
	}

	// a failed run dates nothing, so the next open retries
	dated()
	os.RemoveAll(filepath.Join(d.PluginsRoot(), "marketplaces", "sweep", ".git"))
	if news, _, err := AutoUpdate(t.Context(), d, true); err == nil {
		t.Errorf("a broken clone passed: %v", news)
	}
	if f, _ := readMarketplaces(d); !f.Checked.IsZero() && time.Since(f.Checked) < sweepEvery {
		t.Error("the failed run dated the sweep")
	}
}
