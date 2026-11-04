package lua

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	glua "github.com/yuin/gopher-lua"
)

// mustToGo converts a Lua value or fails the test with the error.
func mustToGo(t *testing.T, v glua.LValue) any {
	t.Helper()
	out, err := ToGo(v)
	if err != nil {
		t.Fatalf("ToGo: unexpected error: %v", err)
	}
	return out
}

func TestToGoScalars(t *testing.T) {
	cases := []struct {
		in   glua.LValue
		want any
	}{
		{glua.LNil, nil},
		{glua.LTrue, true},
		{glua.LFalse, false},
		{glua.LNumber(42), float64(42)},
		{glua.LNumber(-1.5), float64(-1.5)},
		{glua.LString("hi"), "hi"},
	}
	for _, c := range cases {
		if got := mustToGo(t, c.in); got != c.want {
			t.Errorf("ToGo(%v) = %v (%T), want %v", c.in, got, got, c.want)
		}
	}
}

func TestToGoArrayDetection(t *testing.T) {
	L := newTestSandbox(t).L

	arr := L.NewTable()
	for i := 1; i <= 3; i++ {
		arr.RawSetInt(i, glua.LNumber(i*10))
	}
	got := mustToGo(t, arr)
	list, ok := got.([]any)
	if !ok {
		t.Fatalf("contiguous array -> %T, want []any", got)
	}
	if len(list) != 3 || list[0] != float64(10) || list[2] != float64(30) {
		t.Fatalf("array contents = %v", list)
	}

	// A nil hole (t[2] absent) makes it a map, not an array.
	hole := L.NewTable()
	hole.RawSetInt(1, glua.LString("a"))
	hole.RawSetInt(3, glua.LString("c"))
	if _, ok := mustToGo(t, hole).([]any); ok {
		t.Fatal("table with nil hole marshaled as array")
	}

	// A single string key makes it a map.
	mixed := L.NewTable()
	mixed.RawSetInt(1, glua.LNumber(1))
	mixed.RawSetString("k", glua.LString("v"))
	m, ok := mustToGo(t, mixed).(map[string]any)
	if !ok {
		t.Fatalf("mixed-key table -> %T, want map", got)
	}
	if m["1"] != float64(1) || m["k"] != "v" {
		t.Fatalf("mixed map = %v", m)
	}
}

// TestToGoRefusals: cycles, nesting past the cap, oversize strings and
// functions are refused; a table shared by siblings and a chain or string at
// the cap marshal, and userdata passes through as is.
func TestToGoRefusals(t *testing.T) {
	L := newTestSandbox(t).L
	chain := func(n int) *glua.LTable {
		top := L.NewTable()
		for cur, i := top, 0; i < n; i++ {
			next := L.NewTable()
			cur.RawSetInt(1, next)
			cur = next
		}
		return top
	}
	self := L.NewTable()
	self.RawSetString("self", self)
	a, b := L.NewTable(), L.NewTable()
	a.RawSetString("b", b)
	b.RawSetString("a", a)
	leaf, dag := L.NewTable(), L.NewTable()
	dag.RawSetString("x", leaf)
	dag.RawSetString("y", leaf)
	for _, c := range []struct {
		name string
		v    glua.LValue
		err  error // nil: it marshals
	}{
		{"self cycle", self, ErrCycle},
		{"two-table cycle", a, ErrCycle},
		{"shared table", dag, nil},
		{"past the depth cap", chain(MaxTableDepth + 2), ErrTooDeep},
		{"at the depth cap", chain(MaxTableDepth - 1), nil},
		{"past the string cap", glua.LString(strings.Repeat("x", MaxStringBytes+1)), ErrTooLarge},
		{"at the string cap", glua.LString(strings.Repeat("x", MaxStringBytes)), nil},
	} {
		if _, err := ToGo(c.v); !errors.Is(err, c.err) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.err)
		}
	}

	if _, err := ToGo(L.NewFunction(func(*glua.LState) int { return 0 })); err == nil || !strings.Contains(err.Error(), "cannot marshal") {
		t.Fatalf("function refusal = %v", err)
	}
	ud := L.NewUserData()
	if got, err := ToGo(ud); err != nil || got != ud {
		t.Fatalf("userdata passthrough = %v, %v", got, err)
	}
}

func TestFromGoScalars(t *testing.T) {
	L := newTestSandbox(t).L
	cases := []struct {
		in   any
		want glua.LValue
	}{
		{nil, glua.LNil},
		{true, glua.LTrue},
		{"hi", glua.LString("hi")},
		{7, glua.LNumber(7)},
		{int64(-3), glua.LNumber(-3)},
		{float32(2.5), glua.LNumber(float64(float32(2.5)))},
		{1.25, glua.LNumber(1.25)},
		{errors.New("boom"), glua.LString("boom")},
		{stringerValue{}, glua.LString("stringer!")},
		{namedString("ok"), glua.LString("ok")},
	}
	for _, c := range cases {
		if got := FromGo(L, c.in); got != c.want {
			t.Errorf("FromGo(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

type namedString string

type stringerValue struct{}

func (stringerValue) String() string { return "stringer!" }

func TestFromGoContainers(t *testing.T) {
	L := newTestSandbox(t).L

	list := FromGo(L, []any{"a", float64(2), true})
	tb, ok := list.(*glua.LTable)
	if !ok {
		t.Fatalf("[]any -> %T, want table", list)
	}
	if n := tb.Len(); n != 3 {
		t.Fatalf("[]any length = %d, want 3", n)
	}
	if v := tb.RawGetInt(2); v != glua.LNumber(2) {
		t.Fatalf("list[2] = %v", v)
	}

	strs := FromGo(L, []string{"x", "y"})
	if tb, ok := strs.(*glua.LTable); !ok || tb.Len() != 2 || tb.RawGetInt(1) != glua.LString("x") {
		t.Fatalf("[]string -> %v", strs)
	}

	m := FromGo(L, map[string]any{"k": int64(5)})
	tb, ok = m.(*glua.LTable)
	if !ok {
		t.Fatalf("map -> %T, want table", m)
	}
	if v := tb.RawGetString("k"); v != glua.LNumber(5) {
		t.Fatalf("map[k] = %v", v)
	}

	sm := FromGo(L, map[string]string{"a": "b"})
	if tb, ok = sm.(*glua.LTable); !ok || tb.RawGetString("a") != glua.LString("b") {
		t.Fatalf("map[string]string -> %v", sm)
	}
}

// TestFromGoRefusals: cycles, nesting past the cap, oversize strings and
// types outside the plain set raise; a map shared by siblings is no cycle.
func TestFromGoRefusals(t *testing.T) {
	L := newTestSandbox(t).L
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice
	pair := map[string]any{}
	pair["b"] = map[string]any{"a": pair}
	deep := map[string]any{}
	for cursor, i := deep, 0; i < MaxTableDepth+2; i++ {
		next := map[string]any{}
		cursor["n"], cursor = next, next
	}
	big := strings.Repeat("x", MaxStringBytes+1)
	for _, c := range []struct {
		v    any
		want string
	}{
		{cyclic, "cyclic"},
		{cyclicSlice, "cyclic"},
		{pair, "cyclic"},
		{deep, "depth cap"},
		{struct{ X int }{1}, "cannot marshal"},
		{make(chan int), "cannot marshal"},
		{map[int]string{1: "no"}, "cannot marshal"},
		{[]string{big}, "size cap"},
		{map[string]string{"k": big}, "size cap"},
	} {
		assertFromGoRaises(t, L, c.v, c.want)
	}
	leaf := map[string]any{"v": "leaf"}
	got := FromGo(L, map[string]any{"x": leaf, "y": leaf})
	tb, ok := got.(*glua.LTable)
	if !ok {
		t.Fatalf("want a table, got %T", got)
	}
	y, ok := tb.RawGetString("y").(*glua.LTable)
	if !ok || y.RawGetString("v") != glua.LString("leaf") {
		t.Fatal("an acyclic shared reference must marshal")
	}
}

// assertFromGoRaises runs FromGo with recovery: RaiseError panics outside a
// Lua PCall context.
func assertFromGoRaises(t *testing.T, L *glua.LState, v any, want string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("FromGo(%T) did not raise; want %q", v, want)
		}
		if s := fmt.Sprint(r); !strings.Contains(s, want) {
			t.Fatalf("FromGo raised %q, want containing %q", s, want)
		}
	}()
	FromGo(L, v)
}
