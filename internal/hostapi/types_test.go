package hostapi

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	glua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/runtime"
)

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
