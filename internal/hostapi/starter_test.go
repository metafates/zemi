package hostapi

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
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

// TestStarterInitProviderBlock: the provider block, the del line and the
// tier lines uncommented as the comment says work as written, del of a
// builtin whose key this machine does not export is silent, and the tier
// ids are in the catalog, so a catwalk bump that renames them fails here.
func TestStarterInitProviderBlock(t *testing.T) {
	h, r := newTestHost(t)
	h.Builtin = agent.Builtin(func(string) string { return "x" })
	if err := h.ForceReload(t.Context(), []lua.BootStep{h.NamespacesStep()}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(config.StarterInit, "\n")
	start := slices.Index(lines, "-- zemi.provider.add({")
	end := start + slices.Index(lines[start:], "-- })")
	del := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, `-- zemi.provider.del(`) })
	main := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, `-- zemi.config.set("models.main"`) })
	small := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, `-- zemi.config.set("models.small"`) })
	if start < 0 || end < start || del < 0 || main < 0 || small < 0 {
		t.Fatalf("starter provider block not found: %d %d %d %d %d", start, end, del, main, small)
	}
	var src strings.Builder
	for _, l := range append(lines[start:end+1], lines[del], lines[main], lines[small]) {
		src.WriteString(strings.TrimPrefix(l, "-- ") + "\n")
	}
	runUser(t, r, src.String())
	spec, ok := h.Providers.Lookup("local")
	if !ok || spec.BaseURL != "http://localhost:11434/v1" || len(spec.Models) != 1 || spec.Builtin {
		t.Fatalf("provider = %+v, %v", spec, ok)
	}
	for _, tier := range []string{"models.main", "models.small"} {
		if id := h.Settings.Str(tier); !slices.Contains(h.modelIDs(), id) {
			t.Fatalf("%s = %q is not in the catalog", tier, id)
		}
	}
}
