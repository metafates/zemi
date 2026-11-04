package hostapi

import (
	"context"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"

	"github.com/metafates/zemi/internal/agent"
	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/skill"
	"github.com/metafates/zemi/internal/store"
	"github.com/metafates/zemi/internal/tools"
	"github.com/metafates/zemi/runtime"
)

// annotated collects every `function zemi.a.b(` and `---@field x` line of
// the shipped annotations, so a member can be pinned either as a function
// or as a field of its namespace class.
var (
	_reAnnotatedFn    = regexp.MustCompile(`(?m)^function (zemi\.[A-Za-z0-9_.]+)\(`)
	_reAnnotatedClass = regexp.MustCompile(`(?m)^---@class (zemi\.[A-Za-z0-9_.]+)`)
	_reAnnotatedField = regexp.MustCompile(`(?m)^---@field ([A-Za-z0-9_]+)`)
)

// TestTypesCoverAPI fails when a zemi.* member has no annotation, so the
// types an editor (and zemi itself) reads cannot drift from the surface Go
// actually registers.
func TestTypesCoverAPI(t *testing.T) {
	src := typesSource(t)
	declared := map[string]bool{}
	for _, m := range _reAnnotatedFn.FindAllStringSubmatch(src, -1) {
		declared[m[1]] = true
	}
	// A class member covers sub-tables a namespace holds as one value: an
	// annotation field covers the dotted member without a function of its own.
	var class string
	for line := range strings.SplitSeq(src, "\n") {
		if m := _reAnnotatedClass.FindStringSubmatch(line); m != nil {
			class = m[1]
			continue
		}
		if m := _reAnnotatedField.FindStringSubmatch(line); m != nil && class != "" {
			declared[class+"."+m[1]] = true
		}
	}

	for ns, members := range bootedNamespaces(t, true) {
		for _, member := range members {
			path := ns + "." + member
			// zemi.ui.overlay.open covers the overlay sub-table
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
	for ns, members := range bootedNamespaces(t, true) {
		live[ns] = true
		for _, member := range members {
			live[ns+"."+member] = true
			live[ns+"."+trimLast(member)] = true // zemi.ui.overlay, from overlay.open
		}
	}
	for _, m := range _reAnnotatedFn.FindAllStringSubmatch(typesSource(t), -1) {
		if !live[m[1]] {
			t.Errorf("runtime/types/zemi.lua annotates %s, which zemi does not register", m[1])
		}
	}
}

// TestSyncTypesWritesAnnotations: a start leaves the annotations and an
// editor pointed at them, in the config dir and in each local plugin (never a
// clone or a symlinked plugin), drops a module zemi no longer ships, replaces
// an edited editor config, and leaves the file alone when it already says that
// or is a link, whose target is not zemi's.
func TestSyncTypesWritesAnnotations(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	h.Dirs.DataHome, h.Dirs.ConfigHome = t.TempDir(), t.TempDir()
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
	if err := os.Symlink(victim, filepath.Join(root, "local", "shipped", ".emmyrc.json")); err != nil {
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
	for _, c := range []struct{ name, severity string }{
		{".luarc.json", `"undefined-global": "Error!"`}, // lua-language-server's dialect
		{".emmyrc.json", `"undefined-global": "error"`}, // EmmyLua's, which takes no "Error!"
	} {
		data, err := os.ReadFile(filepath.Join(h.Dirs.ConfigHome, c.name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), h.TypesDir()) || !strings.Contains(string(data), c.severity) {
			t.Fatalf("%s does not point at the types dir or raise the bug diagnostics:\n%s", c.name, data)
		}
		for dir, want := range map[string]bool{filepath.Join(root, "local", "mine"): true, filepath.Join(root, "installed", "clone"): false, elsewhere: false} {
			got, err := os.ReadFile(filepath.Join(dir, c.name))
			if (err == nil) != want || want && string(got) != string(data) {
				t.Errorf("%s/%s written = %v, want %v", dir, c.name, err == nil, want)
			}
		}
		if got, _ := os.ReadFile(victim); string(got) != "keep" {
			t.Errorf("a linked %s wrote through to its target: %q", c.name, got)
		}

		if err := os.WriteFile(filepath.Join(h.Dirs.ConfigHome, c.name), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		h.syncTypes()
		if got, _ := os.ReadFile(filepath.Join(h.Dirs.ConfigHome, c.name)); string(got) != string(data) {
			t.Fatalf("an edited %s was not replaced: %s", c.name, got)
		}
		info, err := os.Stat(filepath.Join(h.Dirs.ConfigHome, c.name))
		if err != nil {
			t.Fatal(err)
		}
		h.syncTypes()
		if again, err := os.Stat(filepath.Join(h.Dirs.ConfigHome, c.name)); err != nil || !again.ModTime().Equal(info.ModTime()) {
			t.Fatalf("an unchanged %s was rewritten: %v", c.name, err)
		}
	}
}

// TestTypesParse: a syntax error in the annotations silently costs the
// user every completion in init.lua, and nothing else ever loads the file.
func TestTypesParse(t *testing.T) {
	if _, err := parse.Parse(strings.NewReader(typesSource(t)), "zemi.lua"); err != nil {
		t.Fatalf("runtime/types/zemi.lua does not parse as Lua: %v", err)
	}
}

// TestTypesExamples: every ```lua example in the annotations parses and
// names only members zemi registers (`test.x` is zemi.test.x), so a renamed
// or dropped call fails here.
// NOTE: examples are parsed, not run or type-checked, so a changed argument
// order or field name passes; running them needs origins, stubs and rules
// per example.
func TestTypesExamples(t *testing.T) {
	live := map[string]bool{}
	for ns, members := range bootedNamespaces(t, true) {
		for _, m := range members {
			for p := ns + "." + m; strings.Contains(p, "."); p = trimLast(p) {
				live[p] = true
			}
		}
	}
	var (
		reBlock   = regexp.MustCompile("(?s)--- ```lua\n(.*?)--- ```")
		reComment = regexp.MustCompile(`(?m)^--- ?`)
		reString  = regexp.MustCompile(`"[^"]*"|'[^']*'`)
		reRef     = regexp.MustCompile(`\b(?:zemi|test)(?:\.[A-Za-z_]\w*)+`)
	)
	blocks := reBlock.FindAllStringSubmatch(typesSource(t), -1)
	if len(blocks) == 0 {
		t.Fatal("no examples found")
	}
	for _, b := range blocks {
		code := reComment.ReplaceAllString(b[1], "")
		if _, err := parse.Parse(strings.NewReader(code), "example"); err != nil {
			t.Errorf("an example does not parse: %v\n%s", err, code)
		}
		for _, ref := range reRef.FindAllString(reString.ReplaceAllString(code, `""`), -1) {
			if path, ok := strings.CutPrefix(ref, "test."); ok {
				ref = "zemi.test." + path
			}
			if !live[ref] {
				t.Errorf("an example names %s, which zemi does not register:\n%s", ref, code)
			}
		}
	}
}

func typesSource(t *testing.T) string {
	t.Helper()
	data, err := runtime.Types.ReadFile("types/zemi.lua")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// bootedNamespaces installs the host API on a fresh runtime and reports
// what it registered: namespace -> dotted member paths; with other, the
// members other states hold too: zemi.test, as a plugin test boot registers
// it, and a workflow script's zemi.workflow.
func bootedNamespaces(t *testing.T, other bool) map[string][]string {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	r := lua.NewRuntime(lua.RuntimeOptions{Log: log, RuntimeFS: runtime.FS})
	h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: t.TempDir()})
	t.Cleanup(h.Close)
	var out map[string][]string
	capture := lua.BootStep{Name: "capture", Run: func(_ context.Context, _ *lua.Runtime, g *lua.Generation) error {
		out = g.SB.NamespaceMembers()
		return nil
	}}
	steps := []lua.BootStep{h.NamespacesStep(), capture}
	if other {
		steps = []lua.BootStep{h.NamespacesStep(), (&PluginTest{h: h}).Step(), capture}
	}
	if err := r.Boot(t.Context(), steps); err != nil {
		t.Fatal(err)
	}
	if !other {
		return out
	}
	L, err := (&wfRun{h: h}).open(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer L.Close()
	ns, ok := L.GetGlobal("zemi").(*glua.LTable)
	if !ok {
		t.Fatal("zemi is not a table")
	}
	wf, ok := ns.RawGetString("workflow").(*glua.LTable)
	if !ok {
		t.Fatal("zemi.workflow is not a table")
	}
	wf.ForEach(func(k, _ glua.LValue) {
		out["zemi.workflow"] = append(out["zemi.workflow"], k.String())
	})
	return out
}

// trimLast drops the last dotted segment ("zemi.ui.overlay.open" ->
// "zemi.ui.overlay").
func trimLast(path string) string {
	if i := strings.LastIndex(path, "."); i > 0 {
		return path[:i]
	}
	return path
}

// The names below live in Go and in hand-kept annotations; LuaLS cannot see
// the Go side, so these tests keep the two in step.

func TestTypesEventNames(t *testing.T) {
	assertSameNames(t, "zemi.EventName", annotatedAlias(t, "zemi.EventName"), slices.Collect(maps.Keys(_eventCatalogue)))
}

// TestTypesEnums: each enum table's class, keyed upper-cased, and each
// alias that types the fields taking one list exactly the Go values; each
// booted table holds exactly the class's keys.
func TestTypesEnums(t *testing.T) {
	reConst := regexp.MustCompile(`(?m)^---@field ([A-Z_]+) "([^"]+)"$`)
	booted := bootedNamespaces(t, false)
	for _, c := range []struct {
		alias, table string
		live         []string
	}{
		{"zemi.ProviderKind", "", strs(agent.Kinds)},
		{"zemi.ProviderObjectMode", "", strs(agent.ObjectModes)},
		{"zemi.Level", "", slices.Collect(maps.Keys(_notifyLevels))},
		{"zemi.ServiceStatus", "", strs(_serviceStatuses)},
		{"zemi.TodoStatus", "", strs(store.TodoStatuses)},
		{"zemi.ToolVerdict", "", strs(tools.PreVerdicts)},
		{"zemi.SkillSource", "", strs(skill.Sources)},
		{"zemi.HealthLevel", "", strs(_healthLevels)},
		{"zemi.NArgs", "", strs(_nargsSpecs)},
		{"zemi.ScrollAction", "", strs(_scrollActions)},
		{"zemi.OverlayAnchor", "", strs(_overlayAnchors)},
		{"zemi.MarkKind", "", strs(slices.Collect(maps.Keys(Marks)))},
		{"zemi.TurnEnd", "", strs(_turnEnds)},
		{"zemi.Waiting", "", strs(_waitings)},
		{"zemi.SettingKind", "", strs(config.Kinds)},
	} {
		if c.alias != "" {
			assertSameNames(t, c.alias, annotatedAlias(t, c.alias), slices.Clone(c.live))
		}
		if c.table == "" {
			continue
		}
		var keys, vals, live []string
		for _, m := range reConst.FindAllStringSubmatch(annotatedClass(t, c.table), -1) {
			if m[1] != strings.ToUpper(m[2]) {
				t.Errorf("%s.%s is %q", c.table, m[1], m[2])
			}
			keys, vals = append(keys, m[1]), append(vals, m[2])
		}
		assertSameNames(t, c.table, vals, slices.Clone(c.live))
		for ns, members := range booted {
			for _, m := range members {
				if k, ok := strings.CutPrefix(ns+"."+m, c.table+"."); ok {
					live = append(live, k)
				}
			}
		}
		assertSameNames(t, c.table+" booted", keys, live)
	}
}

func TestTypesHostActions(t *testing.T) {
	t.Parallel()
	h, _ := newTestHost(t)
	var live []string
	for name, fn := range h.hostActions {
		if fn != nil { // zemi.reload is Go-reserved, never a keymap action
			live = append(live, name)
		}
	}
	assertSameNames(t, "zemi.HostAction", annotatedAlias(t, "zemi.HostAction"), live)
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

func assertSameNames(t *testing.T, what string, annotated, live []string) {
	t.Helper()
	slices.Sort(annotated)
	slices.Sort(live)
	if !slices.Equal(annotated, live) {
		t.Errorf("%s annotates %v\nzemi has %v", what, annotated, live)
	}
}
