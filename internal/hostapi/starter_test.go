package hostapi

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
)

var starterCall = regexp.MustCompile(`zemi\.[a-z_][a-z_.]*[a-z_]`)

// TestStarterInitNamesLiveAPI: the starter init.lua is the first Lua most
// users read, and it is all comments, so nothing else catches a member a
// rename took away. Reading one raises with the members the namespace has.
func TestStarterInitNamesLiveAPI(t *testing.T) {
	_, r := newTestHost(t)
	names := starterCall.FindAllString(config.StarterInit, -1)
	slices.Sort(names)
	names = slices.Compact(names)
	if len(names) < 8 {
		t.Fatalf("starter init names %d zemi.* members, expected the examples", len(names))
	}
	var src strings.Builder
	for _, n := range names {
		src.WriteString("assert(" + n + " ~= nil, \"" + n + " is nil\")\n")
	}
	runUser(t, r, src.String())
}

// TestStarterInitProviderBlock: the first provider block uncommented as
// the comment says works as written: nothing in it points the Anthropic
// kind at another endpoint.
func TestStarterInitProviderBlock(t *testing.T) {
	h, r := newTestHost(t)
	lines := strings.Split(config.StarterInit, "\n")
	start := slices.Index(lines, "-- zemi.provider.add({")
	end := start + slices.Index(lines[start:], "-- })")
	main := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, `-- zemi.config.set("models.main"`) })
	if start < 0 || end < start || main < 0 {
		t.Fatalf("starter provider block not found: %d %d %d", start, end, main)
	}
	var src strings.Builder
	for _, l := range append(lines[start:end+1], lines[main]) {
		src.WriteString(strings.TrimPrefix(l, "-- ") + "\n")
	}
	runUser(t, r, src.String())
	spec, ok := h.Providers.Lookup("anthropic")
	if !ok || spec.BaseURL != "" || len(spec.Models) != 1 {
		t.Fatalf("provider = %+v, %v", spec, ok)
	}
	if id := h.Settings.Str("models.main"); id != "anthropic/"+spec.Models[0].ID {
		t.Fatalf("models.main = %q", id)
	}
}
