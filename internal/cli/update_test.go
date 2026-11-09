package cli

import (
	"testing"
)

func TestBaseVersion(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ v, want string }{
		{"v0.1.0+6dd0c22.dirty", "v0.1.0"},
		{"v0.1.0", "v0.1.0"},
		{"dev+6dd0c22", ""},
		{"dev", ""},
		{"", ""},
	} {
		if got := baseVersion(c.v); got != c.want {
			t.Errorf("baseVersion(%q) = %q, want %q", c.v, got, c.want)
		}
	}
}

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
