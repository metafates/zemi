package hostapi

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	zemiruntime "github.com/metafates/zemi/runtime"
	"github.com/yuin/gopher-lua/parse"
)

// annotated collects every `function zemi.a.b(` and `---@field x` line of
// the shipped annotations, so a member can be pinned either as a function
// or as a field of its namespace class.
var (
	reAnnotatedFn    = regexp.MustCompile(`(?m)^function (zemi\.[A-Za-z0-9_.]+)\(`)
	reAnnotatedClass = regexp.MustCompile(`(?m)^---@class (zemi\.[A-Za-z0-9_.]+)`)
	reAnnotatedField = regexp.MustCompile(`(?m)^---@field ([A-Za-z0-9_]+)`)
)

// TestTypesCoverAPI fails when a zemi.* member has no annotation, so the
// types an editor (and zemi itself) reads cannot drift from the surface Go
// actually registers.
func TestTypesCoverAPI(t *testing.T) {
	src := typesSource(t)
	declared := map[string]bool{}
	for _, m := range reAnnotatedFn.FindAllStringSubmatch(src, -1) {
		declared[m[1]] = true
	}
	// A class member covers constants and sub-tables: zemi.provider.kind is
	// a table of strings, not a function.
	var class string
	for line := range strings.SplitSeq(src, "\n") {
		if m := reAnnotatedClass.FindStringSubmatch(line); m != nil {
			class = m[1]
			continue
		}
		if m := reAnnotatedField.FindStringSubmatch(line); m != nil && class != "" {
			declared[class+"."+m[1]] = true
		}
	}

	for ns, members := range bootedNamespaces(t) {
		for _, member := range members {
			path := ns + "." + member
			// zemi.provider.kind.ANTHROPIC is covered by the kind class
			if !declared[path] && !declared[trimLast(path)] {
				t.Errorf("runtime/types/zemi.lua does not annotate %s", path)
			}
		}
	}
}

// TestTypesDeclareNothingExtra: an annotation for a member zemi no longer
// registers is worse than none, because an editor keeps offering it.
func TestTypesDeclareNothingExtra(t *testing.T) {
	live := map[string]bool{"zemi.api.version": true}
	for ns, members := range bootedNamespaces(t) {
		live[ns] = true
		for _, member := range members {
			live[ns+"."+member] = true
			live[ns+"."+trimLast(member)] = true // zemi.ui.widget, from widget.register
		}
	}
	for _, m := range reAnnotatedFn.FindAllStringSubmatch(typesSource(t), -1) {
		if !live[m[1]] {
			t.Errorf("runtime/types/zemi.lua annotates %s, which zemi does not register", m[1])
		}
	}
}

// TestSyncTypesWritesAnnotations: a start leaves the annotations and a
// lua-language-server pointed at them, and never overwrites a .luarc.json
// the user edited.
func TestSyncTypesWritesAnnotations(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.DataHome, h.Dirs.ConfigHome = t.TempDir(), t.TempDir()
	luarc := filepath.Join(h.Dirs.ConfigHome, ".luarc.json")

	h.syncTypes()
	for _, rel := range []string{"zemi.lua", filepath.Join("zemi", "util.lua")} {
		if _, err := os.Stat(filepath.Join(h.TypesDir(), rel)); err != nil {
			t.Fatalf("types/%s: %v", rel, err)
		}
	}
	data, err := os.ReadFile(luarc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), h.TypesDir()) {
		t.Fatalf(".luarc.json does not point at the types dir:\n%s", data)
	}

	if err := os.WriteFile(luarc, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.syncTypes()
	if data, _ := os.ReadFile(luarc); string(data) != "{}" {
		t.Fatalf("an existing .luarc.json was overwritten: %s", data)
	}
}

// TestTypesParse: a syntax error in the annotations silently costs the
// user every completion in init.lua, and nothing else ever loads the file.
func TestTypesParse(t *testing.T) {
	if _, err := parse.Parse(strings.NewReader(typesSource(t)), "zemi.lua"); err != nil {
		t.Fatalf("runtime/types/zemi.lua does not parse as Lua: %v", err)
	}
}

func typesSource(t *testing.T) string {
	t.Helper()
	data, err := zemiruntime.Types.ReadFile("types/zemi.lua")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// bootedNamespaces installs the host API on a fresh runtime and reports
// what it registered: namespace -> dotted member paths.
func bootedNamespaces(t *testing.T) map[string][]string {
	t.Helper()
	log := logging.New(t.TempDir())
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	var out map[string][]string
	capture := lua.BootStep{Name: "capture", Run: func(_ context.Context, _ *lua.Runtime, g *lua.Generation) error {
		out = g.SB.NamespaceMembers()
		return nil
	}}
	if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep(), capture}); err != nil {
		t.Fatal(err)
	}
	return out
}

// trimLast drops the last dotted segment ("zemi.ui.widget.register" ->
// "zemi.ui.widget").
func trimLast(path string) string {
	if i := strings.LastIndex(path, "."); i > 0 {
		return path[:i]
	}
	return path
}
