package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/store"
)

// TestNewerRelease: a build sits at or past the tag its version names, so
// only a strictly greater tag is a newer release; a build with no tag is past
// every tag.
func TestNewerRelease(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		latest, version string
		want            bool
	}{
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0+6dd0c22.dirty", false}, // a dev build is past the tag
		{"v0.1.0", "v0.1.0+6dd0c22", false},
		{"v0.1.1", "v0.1.0+6dd0c22.dirty", true},
		{"v0.1.1", "v0.1.1+6dd0c22.dirty", false}, // past the remembered tag too
		{"v0.1.0", "v0.1.1+6dd0c22.dirty", false},
		{"v0.1.0", "dev+6dd0c22", false},
		{"v0.1.0", "dev", false},
		{"v0.0.9", "v0.1.0", false},
		{"v0.2.0-rc.1", "v0.1.0", true},
		{"", "v0.1.0", false},
		{"garbage", "v0.1.0", false},
	} {
		if got := newerRelease(c.latest, c.version); got != c.want {
			t.Errorf("newerRelease(%q, %q) = %v, want %v", c.latest, c.version, got, c.want)
		}
	}
}

// TestUpdateCheck: a remembered newer release answers without asking, a
// build that caught up drops the memory, a day-old answer stands, and a
// spent day asks the proxy (off here, so the ask says so).
func TestUpdateCheck(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	db, err := store.Open(filepath.Join(t.TempDir(), "zemi.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := db.SetKV(_kvUpdateLatest, strconv.Quote("v99.0.0")); err != nil {
		t.Fatal(err)
	}
	if newer, err := updateCheck(ctx, db, "v0.1.0"); !newer || err != nil {
		t.Fatalf("remembered newer: newer=%v err=%v", newer, err)
	}

	_ = db.SetKV(_kvUpdateSeen, strconv.FormatInt(time.Now().Unix(), 10))
	if newer, err := updateCheck(ctx, db, "v99.1.0"); newer || err != nil {
		t.Fatalf("caught up within the day: newer=%v err=%v", newer, err)
	}
	if _, err := db.GetKV(_kvUpdateLatest); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the stale memory stayed: %v", err)
	}

	_, _ = db.DelKV(_kvUpdateSeen)
	if _, err := updateCheck(ctx, db, "v0.1.0"); err == nil {
		t.Fatal("a spent day with no proxy asked nothing")
	}
}

func TestProxyURL(t *testing.T) {
	for _, c := range []struct {
		goproxy, want string
	}{
		{"", "https://proxy.golang.org"},
		{"https://proxy.example.com,direct", "https://proxy.example.com"},
		{"https://proxy.example.com|fallback,off", "https://proxy.example.com"},
		{"off", ""},
		{"direct", ""},
		{"direct,https://proxy.example.com", ""},
		{"https://proxy.example.com/", "https://proxy.example.com"},
	} {
		t.Setenv("GOPROXY", c.goproxy)
		if got := proxyURL(); got != c.want {
			t.Errorf("GOPROXY=%q: proxyURL() = %q, want %q", c.goproxy, got, c.want)
		}
	}
}
