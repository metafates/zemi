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

func TestToGoCycleRefused(t *testing.T) {
	L := newTestSandbox(t).L
	tb := L.NewTable()
	tb.RawSetString("self", tb)
	if _, err := ToGo(tb); !errors.Is(err, ErrCycle) {
		t.Fatalf("self-referencing table: err = %v, want ErrCycle", err)
	}

	// Two tables in a cycle (a -> b -> a); shared non-cyclic refs are fine.
	a, b := L.NewTable(), L.NewTable()
	a.RawSetString("b", b)
	b.RawSetString("a", a)
	if _, err := ToGo(a); !errors.Is(err, ErrCycle) {
		t.Fatalf("two-table cycle: err = %v, want ErrCycle", err)
	}

	shared := glua.LString("leaf")
	dag := L.NewTable()
	x, y := L.NewTable(), L.NewTable()
	x.RawSetString("s", shared)
	y.RawSetString("s", shared)
	dag.RawSetString("x", x)
	dag.RawSetString("y", y)
	if _, err := ToGo(dag); err != nil {
		t.Fatalf("shared acyclic reference must marshal: %v", err)
	}
}

func TestToGoDepthCap(t *testing.T) {
	L := newTestSandbox(t).L
	deep := L.NewTable()
	cursor := deep
	for range MaxTableDepth + 2 {
		next := L.NewTable()
		cursor.RawSetInt(1, next)
		cursor = next
	}
	if _, err := ToGo(deep); !errors.Is(err, ErrTooDeep) {
		t.Fatalf("depth %d+ chain: err = %v, want ErrTooDeep", MaxTableDepth, err)
	}

	okDepth := L.NewTable()
	cursor = okDepth
	for range MaxTableDepth - 1 {
		next := L.NewTable()
		cursor.RawSetInt(1, next)
		cursor = next
	}
	if _, err := ToGo(okDepth); err != nil {
		t.Fatalf("depth %d chain must marshal: %v", MaxTableDepth-1, err)
	}
}

func TestToGoStringCap(t *testing.T) {
	big := strings.Repeat("x", MaxStringBytes+1)
	if _, err := ToGo(glua.LString(big)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize string: err = %v, want ErrTooLarge", err)
	}
	if _, err := ToGo(glua.LString(strings.Repeat("x", MaxStringBytes))); err != nil {
		t.Fatalf("string at cap must marshal: %v", err)
	}
}

func TestToGoRefusals(t *testing.T) {
	L := newTestSandbox(t).L

	if _, err := ToGo(L.NewFunction(func(*glua.LState) int { return 0 })); err == nil {
		t.Fatal("function must be refused")
	} else if !strings.Contains(err.Error(), "cannot marshal") {
		t.Fatalf("function refusal message = %v", err)
	}

	ud := L.NewUserData()
	ud.Value = 123
	got, err := ToGo(ud)
	if err != nil {
		t.Fatalf("userdata must pass through opaque: %v", err)
	}
	if got != ud {
		t.Fatalf("userdata passthrough = %v, want identity", got)
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
	}
	for _, c := range cases {
		if got := FromGo(L, c.in); got != c.want {
			t.Errorf("FromGo(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

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

func TestFromGoCycleRefused(t *testing.T) {
	L := newTestSandbox(t).L

	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	assertFromGoRaises(t, L, cyclic, "cyclic")

	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice
	assertFromGoRaises(t, L, cyclicSlice, "cyclic")

	// Two maps referencing each other are also a cycle.
	a := map[string]any{}
	b := map[string]any{"a": a}
	a["b"] = b
	assertFromGoRaises(t, L, a, "cyclic")

	// The same map appearing twice as siblings is a DAG, not a cycle.
	leaf := map[string]any{"v": "leaf"}
	dag := map[string]any{"x": leaf, "y": leaf}
	tb, ok := FromGo(L, dag).(*glua.LTable)
	if !ok {
		t.Fatal("acyclic shared reference must marshal")
	}
	if len(tb.RawGetString("x").(*glua.LTable).RawGetString("v").(glua.LString)) == 0 {
		t.Fatal("shared leaf content missing")
	}
}

func TestFromGoDepthCap(t *testing.T) {
	L := newTestSandbox(t).L
	deep := any(map[string]any{})
	cursor := deep.(map[string]any)
	for range MaxTableDepth + 2 {
		next := map[string]any{}
		cursor["n"] = next
		cursor = next
	}
	assertFromGoRaises(t, L, deep, "depth cap")
}

func TestFromGoUnsupportedRefused(t *testing.T) {
	L := newTestSandbox(t).L
	assertFromGoRaises(t, L, struct{ X int }{1}, "cannot marshal")
	assertFromGoRaises(t, L, make(chan int), "cannot marshal")
	assertFromGoRaises(t, L, map[int]string{1: "no"}, "cannot marshal")
	big := strings.Repeat("x", MaxStringBytes+1)
	assertFromGoRaises(t, L, []string{big}, "size cap")
	assertFromGoRaises(t, L, map[string]string{"k": big}, "size cap")
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
