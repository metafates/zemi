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
