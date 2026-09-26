package hostapi

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/skills"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
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
			live[ns+"."+trimLast(member)] = true // zemi.ui.widget, from widget.add
		}
	}
	for _, m := range reAnnotatedFn.FindAllStringSubmatch(typesSource(t), -1) {
		if !live[m[1]] {
			t.Errorf("runtime/types/zemi.lua annotates %s, which zemi does not register", m[1])
		}
	}
}

// TestSyncTypesWritesAnnotations: a start leaves the annotations and a
// lua-language-server pointed at them, in the config dir and in each local
// plugin (never a clone or a symlinked plugin), drops a module zemi no
// longer ships, replaces an edited .luarc.json, and leaves the file alone
// when it already says that or is a link, whose target is not zemi's.
func TestSyncTypesWritesAnnotations(t *testing.T) {
	h, _ := newTestHost(t)
	h.Dirs.DataHome, h.Dirs.ConfigHome = t.TempDir(), t.TempDir()
	luarc := filepath.Join(h.Dirs.ConfigHome, ".luarc.json")
	root, elsewhere := h.Dirs.PluginsRoot(), t.TempDir()
	for _, d := range []string{"local/mine", "local/shipped", "installed/clone"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "local", "linked")); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "zshrc")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(root, "local", "shipped", ".luarc.json")); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(h.TypesDir(), "zemi", "gone.lua")
	if err := os.MkdirAll(filepath.Dir(gone), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gone, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	h.syncTypes()
	for _, rel := range []string{"zemi.lua", filepath.Join("zemi", "util.lua")} {
		if _, err := os.Stat(filepath.Join(h.TypesDir(), rel)); err != nil {
			t.Fatalf("types/%s: %v", rel, err)
		}
	}
	if _, err := os.Stat(gone); err == nil {
		t.Error("a module zemi no longer ships is still in the types dir")
	}
	data, err := os.ReadFile(luarc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), h.TypesDir()) {
		t.Fatalf(".luarc.json does not point at the types dir:\n%s", data)
	}
	for dir, want := range map[string]bool{filepath.Join(root, "local", "mine"): true, filepath.Join(root, "installed", "clone"): false, elsewhere: false} {
		got, err := os.ReadFile(filepath.Join(dir, ".luarc.json"))
		if (err == nil) != want || want && string(got) != string(data) {
			t.Errorf("%s/.luarc.json written = %v, want %v", dir, err == nil, want)
		}
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Errorf("a linked .luarc.json wrote through to its target: %q", got)
	}

	if err := os.WriteFile(luarc, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.syncTypes()
	if got, _ := os.ReadFile(luarc); string(got) != string(data) {
		t.Fatalf("an edited .luarc.json was not replaced: %s", got)
	}
	info, err := os.Stat(luarc)
	if err != nil {
		t.Fatal(err)
	}
	h.syncTypes()
	if again, err := os.Stat(luarc); err != nil || !again.ModTime().Equal(info.ModTime()) {
		t.Fatalf("an unchanged .luarc.json was rewritten: %v", err)
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

// trimLast drops the last dotted segment ("zemi.ui.widget.add" ->
// "zemi.ui.widget").
func trimLast(path string) string {
	if i := strings.LastIndex(path, "."); i > 0 {
		return path[:i]
	}
	return path
}

// The names below live in Go and in hand-kept annotations; LuaLS cannot see
// the Go side, so these tests keep the two in step.

func TestTypesEventNames(t *testing.T) {
	assertSameNames(t, "zemi.EventName", annotatedAlias(t, "zemi.EventName"), slices.Collect(maps.Keys(eventCatalogue)))
}

// TestTypesEnums: each enum table's class, keyed upper-cased, and each
// alias that types the fields taking one list exactly the Go values.
func TestTypesEnums(t *testing.T) {
	reConst := regexp.MustCompile(`(?m)^---@field ([A-Z_]+) "([^"]+)"$`)
	for _, c := range []struct {
		alias, table string
		live         []string
	}{
		{"zemi.ProviderKind", "zemi.provider.kind", strs(agent.Kinds)},
		{"zemi.Decision", "zemi.permission.decision", []string{string(perm.Allow), string(perm.Ask), string(perm.Deny)}},
		{"zemi.Scope", "zemi.memory.scope", strs(store.Scopes)},
		{"zemi.Level", "zemi.notify.level", slices.Collect(maps.Keys(notifyLevels))},
		{"zemi.ModelRole", "zemi.model.role", roleNames()},
		{"", "zemi.model.effort", strs(agent.Efforts)},
		{"zemi.ServiceStatus", "", strs(serviceStatuses)},
		{"zemi.MemoryOrigin", "", strs(store.Origins)},
		{"zemi.TodoStatus", "", strs(store.TodoStatuses)},
		{"zemi.ToolVerdict", "", strs(tools.PreVerdicts)},
		{"zemi.SkillSource", "", strs(skills.Sources)},
		{"zemi.Role", "", strs([]fantasy.MessageRole{fantasy.MessageRoleUser, fantasy.MessageRoleAssistant})},
		{"zemi.LearnAction", "", learnActions},
		{"zemi.HealthLevel", "", strs(healthLevels)},
		{"zemi.Underline", "", strs(underlines)},
		{"zemi.NArgs", "", strs(nargsSpecs)},
		{"zemi.ScrollAction", "", strs(scrollActions)},
		{"zemi.OverlayAnchor", "", strs(overlayAnchors)},
		{"zemi.LayoutDir", "", strs(layoutDirs)},
		{"zemi.LayoutScroll", "", strs(scrollEdges)},
		{"zemi.FloatAnchor", "", strs(floatAnchors)},
		{"zemi.RowKind", "", strs(rowKinds)},
		{"zemi.CallStatus", "", strs(callStatuses)},
		{"zemi.TurnEnd", "", strs(turnEnds)},
		{"zemi.Waiting", "", strs(waitings)},
		{"zemi.RunStatus", "", strs(runStatuses)},
		{"zemi.SettingKind", "", strs(config.Kinds)},
	} {
		if c.alias != "" {
			assertSameNames(t, c.alias, annotatedAlias(t, c.alias), slices.Clone(c.live))
		}
		if c.table == "" {
			continue
		}
		var vals []string
		for _, m := range reConst.FindAllStringSubmatch(annotatedClass(t, c.table), -1) {
			if m[1] != strings.ToUpper(m[2]) {
				t.Errorf("%s.%s is %q", c.table, m[1], m[2])
			}
			vals = append(vals, m[2])
		}
		assertSameNames(t, c.table, vals, slices.Clone(c.live))
	}
}

func TestTypesHostActions(t *testing.T) {
	h, _ := newTestHost(t)
	var live []string
	for name, fn := range h.hostActions {
		if fn != nil { // zemi.reload is Go-reserved, never a keymap action
			live = append(live, name)
		}
	}
	assertSameNames(t, "zemi.HostAction", annotatedAlias(t, "zemi.HostAction"), live)
}

// TestTypesWidgetCtx: every key a render's ctx holds is annotated, and every
// annotated field that is not optional reads non-nil (the lazy ones too).
func TestTypesWidgetCtx(t *testing.T) {
	h, r := newTestHost(t)
	var fields strings.Builder
	for _, m := range regexp.MustCompile(`(?m)^---@field ([a-z_]+)(\??) `).FindAllStringSubmatch(annotatedClass(t, "zemi.WidgetCtx"), -1) {
		fmt.Fprintf(&fields, "%s = %t, ", m[1], m[2] == "?")
	}
	runUser(t, r, `
		local fields = { `+fields.String()+` }
		zemi.ui.widget.add({ id = "types", region = "header", render = function(ctx)
			local raw = {}
			for k in pairs(ctx) do raw[#raw + 1] = k end
			for _, k in ipairs(raw) do
				assert(fields[k] ~= nil, "zemi.WidgetCtx does not annotate ctx." .. k)
			end
			for k, optional in pairs(fields) do
				assert(optional or ctx[k] ~= nil, "zemi.WidgetCtx annotates ctx." .. k .. ", which ctx does not set")
			end
			return {}
		end })`)
	if _, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("types"), 40, 10, UIState{}); err != nil {
		t.Fatal(err)
	}
}

// annotatedClass is the block of `---@field` lines under `---@class name`.
func annotatedClass(t *testing.T, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(typesSource(t), "---@class "+name+"\n")
	if !ok {
		t.Fatalf("runtime/types/zemi.lua has no class %s", name)
	}
	var b strings.Builder
	for line := range strings.SplitSeq(rest, "\n") {
		if !strings.HasPrefix(line, "---@field ") {
			break
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

// annotatedAlias is the quoted members of `---@alias name` and its `---|`
// lines, "" included, their `#` comments left out.
func annotatedAlias(t *testing.T, name string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(typesSource(t), "---@alias "+name+"\n")
	if !ok {
		t.Fatalf("runtime/types/zemi.lua has no alias %s", name)
	}
	var out []string
	for line := range strings.SplitSeq(rest, "\n") {
		if !strings.HasPrefix(line, "---|") {
			break
		}
		line, _, _ = strings.Cut(line, "#")
		for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// TestTypesLayoutKinds: zemi.LayoutNode is a union of one class per kind,
// each with its own `kind` literal, which together are the kinds Go parses.
func TestTypesLayoutKinds(t *testing.T) {
	_, rest, _ := strings.Cut(typesSource(t), "---@alias zemi.LayoutNode\n")
	var kinds []string
	for line := range strings.SplitSeq(rest, "\n") {
		class, ok := strings.CutPrefix(line, "---| ")
		if !ok {
			break
		}
		m := regexp.MustCompile(`(?m)^---@field kind "([a-z]+)"$`).FindStringSubmatch(annotatedClass(t, class+": zemi.LayoutBase"))
		if m == nil {
			t.Fatalf("%s has no kind literal", class)
		}
		kinds = append(kinds, m[1])
	}
	assertSameNames(t, "zemi.LayoutNode kinds", kinds, strs(layoutKinds))
}

func assertSameNames(t *testing.T, what string, annotated, live []string) {
	t.Helper()
	slices.Sort(annotated)
	slices.Sort(live)
	if !slices.Equal(annotated, live) {
		t.Errorf("%s annotates %v\nzemi has %v", what, annotated, live)
	}
}
