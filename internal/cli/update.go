package cli

// update.go is the release check: once a day, opening the TUI, zemi asks the
// module proxy whether a newer tag exists, remembers one and says so when it
// exits; `zemi update` installs the latest tag. A build is at or past the tag
// its version names (v0.1.0+6dd0c22.dirty), so only a strictly greater tag
// moves it. No version is ever named to the user: the remembered one can go
// stale, the installed one is what the proxy resolves at install time.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/proc"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/ui"
)

const (
	_module         = "github.com/metafates/zemi"
	_kvUpdateLatest = "update.latest"  // a semver tag, quoted
	_kvUpdateSeen   = "update.checked" // unix seconds of the last proxy answer
	_updateEvery    = 24 * time.Hour
)

// checkForUpdate runs on a boot goroutine: at most once a day, and never
// while a newer version is already remembered, it asks the proxy. Every
// failure is silent — the check tries again the next open.
func checkForUpdate(h *hostapi.Host) {
	if h.GlobalDB == nil || !h.Settings.Bool(config.KeyUpdatesCheck) {
		return
	}
	cur := h.Version
	if raw, err := h.GlobalDB.GetKV(_kvUpdateLatest); err == nil {
		if latest, uerr := strconv.Unquote(raw); uerr == nil && !newerRelease(latest, cur) {
			_, _ = h.GlobalDB.DelKV(_kvUpdateLatest) // this build caught up; checking resumes
		}
		return // a newer version is already remembered: it installs before we ask again
	} else if !errors.Is(err, store.ErrNotFound) {
		return
	}
	if raw, err := h.GlobalDB.GetKV(_kvUpdateSeen); err == nil {
		if t, perr := strconv.ParseInt(raw, 10, 64); perr == nil && time.Since(time.Unix(t, 0)) < _updateEvery {
			return
		}
	}
	latest, err := proxyLatest(context.Background())
	if err != nil {
		return
	}
	_ = h.GlobalDB.SetKV(_kvUpdateSeen, strconv.FormatInt(time.Now().Unix(), 10))
	if newerRelease(latest, h.Version) {
		_ = h.GlobalDB.SetKV(_kvUpdateLatest, strconv.Quote(latest))
	}
}

// newerRelease says whether latest is a release this build should move to:
// the build answers to its base tag plus an optional commit suffix
// (v0.1.0+6dd0c22.dirty), so it is at or past that tag already, and a build
// with no tag ("dev+6dd0c22") is past every tag the proxy can name. Only a
// strictly greater tag counts; prerelease tags order below their release.
func newerRelease(latest, version string) bool {
	base := baseVersion(version)
	return base != "" && semver.IsValid(latest) && semver.Compare(latest, base) > 0
}

// updatePending says whether a remembered release is still newer than this
// build: the exit notice prints while it is.
func updatePending(h *hostapi.Host) bool {
	if h.GlobalDB == nil {
		return false
	}
	raw, err := h.GlobalDB.GetKV(_kvUpdateLatest)
	if err != nil {
		return false
	}
	latest, err := strconv.Unquote(raw)
	return err == nil && newerRelease(latest, h.Version)
}

// baseVersion strips what the build added to its tag (v0.1.0+6dd0c22.dirty);
// a build with no tag reads "" and is never compared.
func baseVersion(v string) string {
	if tag, _, ok := strings.Cut(v, "+"); ok {
		v = tag
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// proxyLatest asks the module proxy's @latest, the same answer
// `go install ...@latest` resolves against.
func proxyLatest(ctx context.Context) (string, error) {
	base := proxyURL()
	if base == "" {
		return "", errors.New("GOPROXY is off")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+_module+"/@latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the proxy answered %s", resp.Status)
	}
	var latest struct{ Version string }
	if err := json.NewDecoder(resp.Body).Decode(&latest); err != nil {
		return "", err
	}
	if !semver.IsValid(latest.Version) {
		return "", fmt.Errorf("the proxy named %q", latest.Version)
	}
	return latest.Version, nil
}

// proxyURL reads the first proxy of GOPROXY, the built-in one when the
// variable is unset; direct and off name no proxy.
func proxyURL() string {
	gp := os.Getenv("GOPROXY")
	if gp == "" {
		return "https://proxy.golang.org"
	}
	p, _, _ := strings.Cut(gp, ",")
	p, _, _ = strings.Cut(p, "|")
	if p == "" || p == "direct" || p == "off" {
		return ""
	}
	return strings.TrimSuffix(p, "/")
}

// updateCommand is `zemi update`: go install the latest tag, then say whether
// the binary the user runs is the one that got written.
func updateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "install the latest tagged zemi",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runUpdate(c)
		},
	}
}

func runUpdate(c *cobra.Command) error {
	say := c.OutOrStdout()
	cur := versionString()
	if baseVersion(cur) == "" {
		fmt.Fprintln(say, ui.Chip(say, "update", "this build is not from a tagged release; nothing to update"))
		return nil
	}
	if latest, err := proxyLatest(c.Context()); err == nil && !newerRelease(latest, cur) {
		fmt.Fprintln(say, ui.Chip(say, "update", "zemi is up to date"))
		return nil
	} // the proxy ask failed: install anyway, @latest resolves it again
	go_, err := exec.LookPath("go")
	if err != nil {
		return errors.New("go is not on PATH; install zemi with `go install " + _module + "@latest`")
	}
	env := runGo(c, go_, "env", "GOBIN", "GOPATH")
	if env.err != nil {
		return fmt.Errorf("go env: %w\n%s", env.err, env.out)
	}
	fields := strings.Fields(env.out)
	gobin := filepath.Join(fields[len(fields)-1], "bin") // GOBIN unset: $GOPATH/bin
	if len(fields) == 2 {
		gobin = fields[0]
	}
	if out := runGo(c, go_, "install", _module+"@latest"); out.err != nil {
		return fmt.Errorf("%w\n%s", out.err, out.out)
	}
	fmt.Fprintln(say, ui.Chip(say, "update", "installed the latest zemi; restart zemi to run it"))
	if here, eerr := os.Executable(); eerr == nil && filepath.Clean(here) != filepath.Clean(filepath.Join(gobin, "zemi")) {
		fmt.Fprintln(say, ui.Chip(say, "update", "this run came from "+here+"; the new binary went to "+filepath.Join(gobin, "zemi")))
	}
	return nil
}

// runGo runs one go command to completion, its output on one string.
func runGo(c *cobra.Command, go_, verb string, args ...string) struct {
	out string
	err error
} {
	cmd := proc.CommandContext(c.Context(), go_, verb)
	cmd.Args = append(cmd.Args, args...)
	var b strings.Builder
	cmd.Stdout, cmd.Stderr = &b, &b
	err := proc.Run(cmd)
	return struct {
		out string
		err error
	}{strings.TrimSpace(b.String()), err}
}
