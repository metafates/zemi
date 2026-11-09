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
