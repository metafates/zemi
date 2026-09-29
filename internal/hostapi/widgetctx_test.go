package hostapi

import (
	"context"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	glua "github.com/yuin/gopher-lua"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/logging"
	"github.com/metafates/zemi/internal/lua"
	"github.com/metafates/zemi/internal/store"
)

// TestRelpath: a chat row writes a path under the working directory
// relative to it and one elsewhere under home from ~, as ctx.cwd reads.
func TestRelpath(t *testing.T) {
	runUser(t, newTestHostUI(t).RT, `
local relpath = require("zemi.tui.text").relpath
for _, c in ipairs({
  { "/h/me/p/a.go", "~/p", "/h/me", "a.go" },
  { "cat /h/me/.x/b /h/me/p/c", "~/p", "/h/me", "cat ~/.x/b c" },
  { "/h/meta/a", "~/p", "/h/me", "/h/meta/a" },
  { "/tmp/p/a.go", "/tmp/p", "/h/me", "a.go" },
}) do
  local got = relpath(c[1], c[2], c[3])
  assert(got == c[4], c[1] .. " reads " .. got .. ", want " .. c[4])
end
`)
}

// BenchmarkWidgetCtx renders a statusline-like widget that reads one field
// over an empty and a 500-row transcript: the gap is what building the
// transcript for widgets that never read it costs.
func BenchmarkWidgetCtx(b *testing.B) {
	for _, rows := range []int{0, 500} {
		b.Run(strconv.Itoa(rows), func(b *testing.B) {
			log := logging.New(b.TempDir())
			r := lua.NewRuntime(lua.RuntimeOptions{Log: log})
			h := NewHost(r, log, config.NewStore(), config.Dirs{ProjectDir: b.TempDir()})
			if err := r.Boot(context.Background(), []lua.BootStep{h.NamespacesStep()}); err != nil {
				b.Fatal(err)
			}
			if err := r.ExecuteChunk(context.Background(), `zemi.ui.widget.add({ id = "m", region = "statusline.right", render = function(ctx) return { { { text = ctx.model } } } end })`, "b.lua", lua.OriginUser); err != nil {
				b.Fatal(err)
			}
			diff := strings.Repeat("- old line of code here\n+ new line of code here\n", 20)
			for i := range rows {
				h.AppendChat(ChatMessage{Role: "assistant", Kind: "tool", Tool: "edit", Text: "internal/x.go", Label: "edit x", Status: "ok", Diff: diff, Output: "done\nok"})
				if i%2 == 0 {
					h.AppendChat(ChatMessage{Role: "user", Text: "please change the thing in the other place " + strconv.Itoa(i)})
				}
			}
			w := h.Widgets.Get("m")
			for b.Loop() {
				if _, _, err := h.RenderWidget(context.Background(), w, 40, 1, UIState{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// TestPrintedRender: a frame zemi run prints draws the rows it is handed
// and no pointer at a key or a click; the splash stays out of it.
func TestPrintedRender(t *testing.T) {
	h := newTestHostUI(t)
	rows := []ChatMessage{
		{Role: "assistant", Kind: RowReasoning, Text: "hmm", Stat: &TurnStat{ThoughtMS: 1200}},
		{Role: "assistant", Kind: RowTool, Tool: "bash", Text: "go test", Status: CallErr, Output: "FAIL\nexit status 1"},
		{Role: "assistant", Text: "it fails"},
	}
	h.setTranscript("t", rows)
	chat := func(state UIState) string {
		var b strings.Builder
		for _, w := range h.Widgets.InRegion("chat") {
			lines, _, err := h.RenderWidget(t.Context(), w, 80, 30, state)
			if err != nil {
				t.Fatal(err)
			}
			for _, l := range lines {
				for _, s := range l {
					b.WriteString(s.Text)
				}
				b.WriteString("\n")
			}
		}
		return b.String()
	}
	for _, c := range []struct {
		name  string
		state UIState
		hint  bool
	}{
		{"the TUI points at ctrl+o", UIState{}, true},
		{"a printed frame does not", UIState{Printed: &Printed{Rows: rows}}, false},
	} {
		got := chat(c.state)
		if strings.Contains(got, "ctrl+o") != c.hint || !strings.Contains(got, "exit status 1") || !strings.Contains(got, "it fails") {
			t.Errorf("%s:\n%s", c.name, got)
		}
	}
	if got := chat(UIState{Printed: &Printed{}}); strings.TrimSpace(got) != "" {
		t.Errorf("a printed frame with no rows draws:\n%s", got)
	}
}

// TestWidgetCtxLazy: the built-on-read fields work inside render, and a
// ctx kept past it keeps what render read and reads nil for the rest.
func TestWidgetCtxLazy(t *testing.T) {
	h, r := newTestHost(t)
	h.AppendMessage("user", "hi")
	runUser(t, r, `
		local kept
		zemi.ui.widget.add({ id = "k", region = "header", render = function(ctx)
			assert(#ctx.transcript == 1 and ctx.transcript_count == 1 and ctx.transcript[1].text == "hi", "transcript")
			assert(type(ctx.agent.tools) == "table" and ctx.recent == nil, "lazy fields")
			kept = ctx
			return {}
		end })
		zemi.command.add("check", function()
			assert(kept.transcript ~= nil and kept.agent ~= nil and kept.width == 10, "what render read stays")
			assert(kept.todos == nil, "the rest reads nil")
		end)`)
	if _, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("k"), 10, 1, UIState{}); err != nil {
		t.Fatal(err)
	}
	runUser(t, r, `assert(zemi.command.run("check"))`)
}

// TestConvertSegments: the two returns that unambiguously mean one line
// are wrapped, and the ambiguous one names both spellings (DESIGN §6.0).
func TestConvertSegments(t *testing.T) {
	seg := func(text string) map[string]any { return map[string]any{"text": text} }
	for _, c := range []struct {
		name string
		in   any
		want string // rendered text per line, joined by "|"; "!" prefixes an expected error
	}{
		{"lines", []any{[]any{"a"}, []any{"b"}}, "a|b"},
		{"bare string", "a", "a"},
		{"bare line", []any{seg("a"), seg("b")}, "ab"},
		{"one string", []any{"a"}, "a"},
		{"empty table", map[string]any{}, ""},
		{"strings", []any{"a", "b"}, "!list of strings"},
		{"single segment", seg("a"), "!widget render must return a list of lines, got a single segment"},
		{"number line", []any{42.0}, "!widget line must be a list of segments, got a number"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rows, err := convertSegments(c.in, "w")
			if want, isErr := strings.CutPrefix(c.want, "!"); isErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want %q", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var lines []string
			for _, row := range rows {
				var b strings.Builder
				for _, s := range row {
					b.WriteString(s.Text)
				}
				lines = append(lines, b.String())
			}
			if got := strings.Join(lines, "|"); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestTableSegments: the direct path gives what ToGo and convertSegments
// give, sanitizing included, leaves odd shapes to them, and converts a line
// table it saw in the last render only once.
func TestTableSegments(t *testing.T) {
	h, r := newTestHost(t)
	runUser(t, r, `
		local kept = { { text = "kept" } }
		local shapes = {
			{ { "a\27[31m", { text = "b\27]0;x\7", hl = "Host:BtnSel", click = "go", copy = false, hover = false, soft_wrap = true, effect = "shimmer" } }, kept },
			{ kept },
			{ {} },
			{ { { "nested" } } },
			{ { "a", x = 1 } },
			"bare",
		}
		local n = 0
		zemi.ui.widget.add({ id = "s", region = "header", render = function()
			n = n + 1
			if n == 2 then kept[1].text = "changed" end
			return shapes[n]
		end })`)
	w := h.Widgets.Get("s")
	for i, direct := range []bool{true, true, false, false, false, false} {
		var got [][]Segment
		_, err := h.RT.Do(t.Context(), lua.ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
			if err := L.CallByParam(glua.P{Fn: w.Render, NRet: 1, Protect: true}); err != nil {
				return nil, err
			}
			ret := L.Get(-1)
			L.Pop(1)
			lines, ok := w.tableSegments(ret)
			if ok != direct {
				t.Errorf("shape %d: direct = %v, want %v", i+1, ok, direct)
			}
			if v, err := lua.ToGo(ret); ok && err == nil {
				got, _ = convertSegments(v, w.ID)
				if i == 0 && (!reflect.DeepEqual(lines, got) || strings.ContainsRune(lines[0][0].Text+lines[0][1].Text, 0x1b)) {
					t.Errorf("direct %+v, generic %+v", lines, got)
				}
				got = lines
			}
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && got[0][0].Text != "kept" {
			t.Errorf("a line table seen last render was converted again: %q", got[0][0].Text)
		}
	}
}

// TestIncludedCached: ctx.included reads the recipes dir and the store once,
// not per render, and again after a memory write through this host.
func TestIncludedCached(t *testing.T) {
	h, r := newTestHostSkills(t)
	h.SetProjectTrusted(true)
	runUser(t, r, `zemi.ui.widget.add({ id = "inc", region = "header", render = function(ctx)
		return { { { text = ctx.included.memories .. " " .. ctx.included.skills } } }
	end })`)
	for _, c := range []struct {
		name string
		do   func()
		want string
	}{
		{"first render", func() {}, "0 0"},
		{"a recipe or another handle's memory is not reread", func() {
			writeSkill(t, h.recipesDir(), "rig", "recipe rig", "")
			other, err := store.Open(filepath.Join(h.Dirs.StateHome, "zemi.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if err := other.AddMemory(&store.Memory{Scope: store.ScopeUser, Text: "the user works late", ProvenanceSessionID: "seed", Origin: store.OriginLearned}); err != nil {
				t.Fatal(err)
			}
		}, "0 0"},
		{"a memory write reads again", func() { learnedMemory(t, h, store.ScopeUser, "the user prefers short replies", "") }, "2 1"},
	} {
		c.do()
		lines, _, err := h.RenderWidget(t.Context(), h.Widgets.Get("inc"), 20, 1, UIState{})
		if err != nil {
			t.Fatal(err)
		}
		if got := lines[0][0].Text; got != c.want {
			t.Fatalf("%s: memories and skills %q, want %q", c.name, got, c.want)
		}
	}
}
