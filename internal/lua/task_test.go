package lua

import (
	"context"
	"strings"
	"testing"
	"time"

	glua "github.com/yuin/gopher-lua"
)

// TestTask pins DESIGN §4's task rule: wait() suspends a task and frees the
// interpreter, and raises anywhere a yield could not come back.
func TestTask(t *testing.T) {
	cases := []struct {
		name, src string
		plain     bool   // run as an ordinary request, not a task
		detach    bool   // StartTask instead of RunTask
		act       string // while it waits: "release", "reload" or "cancel"
		want      string // the result, or the error it contains
	}{
		{name: "wait suspends and resumes with what the waiter gave", src: `return zemi.t.wait()`, act: "release", want: "done"},
		{name: "a detached task goes on after its caller returns", src: `zemi.t.wait()`, detach: true, act: "release"},
		{name: "a purge ends a waiting task", src: `zemi.t.wait()`, act: "reload", want: "reloaded"},
		{name: "ctx cancel ends a waiting task", src: `zemi.t.wait()`, act: "cancel", want: "context canceled"},
		{name: "not a task", src: `zemi.t.wait()`, plain: true, want: "only a task can wait"},
		{name: "under pcall", src: `local _, err = pcall(zemi.t.wait) error(err, 0)`, want: "cannot wait inside pcall"},
		{name: "in a library callback", src: `table.sort({ 2, 1 }, function(a, b) zemi.t.wait() return a < b end)`, want: "cannot wait inside pcall"},
		{name: "in a coroutine the task made", src: `coroutine.wrap(function() zemi.t.wait() end)()`, want: "only a task can wait"},
		{name: "coroutine.yield", src: `coroutine.yield()`, want: "coroutine.yield cannot suspend"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			waiting, release := make(chan struct{}, 1), make(chan struct{})
			steps := []BootStep{{Name: "t", Run: func(_ context.Context, _ *Runtime, g *Generation) error {
				tb := g.L.NewTable()
				tb.RawSetString("wait", Suspending(g.L, g.L.NewFunction(func(L *glua.LState) int {
					return Suspend(L, "t.wait", func(ctx context.Context) func(*glua.LState) []glua.LValue {
						waiting <- struct{}{}
						select {
						case <-release:
							return func(*glua.LState) []glua.LValue { return []glua.LValue{glua.LString("done")} }
						case <-ctx.Done():
							return func(*glua.LState) []glua.LValue { return nil }
						}
					})
				})))
				return g.SB.RegisterNamespace("zemi.t", tb)
			}}}
			r := NewRuntime(RuntimeOptions{})
			if err := r.Boot(context.Background(), steps); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { close(r.current().stop) })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			task := Task{Class: ClassAgent, Origin: OriginUser, Path: "t.lua",
				Start: func(L *glua.LState) (*glua.LFunction, []glua.LValue, error) {
					fn, err := genOf(L).SB.CompileChunk(c.src, OriginUser.ChunkName("t.lua"))
					return fn, nil, err
				},
				Done: func(L *glua.LState, rets []glua.LValue) ([]any, error) { return []any{rets[0].String()}, nil },
			}
			type result struct {
				vals []any
				err  error
			}
			done := make(chan result, 1)
			switch {
			case c.plain:
				go func() {
					_, err := r.Do(ctx, ClassUI, 0, 0, func(L *glua.LState) ([]any, error) {
						fn, _, err := task.Start(L)
						if err != nil {
							return nil, err
						}
						L.Push(fn)
						return nil, L.PCall(0, 0, nil)
					})
					done <- result{err: err}
				}()
			case c.detach:
				if err := r.StartTask(ctx, task, func(err error) { done <- result{err: err} }); err != nil {
					t.Fatalf("StartTask: %v", err)
				}
			default:
				go func() {
					vals, err := r.RunTask(ctx, task)
					done <- result{vals, err}
				}()
			}
			if c.act != "" {
				<-waiting
				// the interpreter is free while the task waits
				if _, err := r.Do(context.Background(), ClassUI, 0, 0, func(*glua.LState) ([]any, error) { return nil, nil }); err != nil {
					t.Fatalf("a request while the task waits: %v", err)
				}
				switch c.act {
				case "release":
					close(release)
				case "reload":
					if err := r.ForceReload(context.Background(), steps, nil); err != nil {
						t.Fatal(err)
					}
				case "cancel":
					cancel()
				}
			}
			var res result
			select {
			case res = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("the task did not end within 3s")
			}
			got := ""
			if res.err != nil {
				got = res.err.Error()
			} else if len(res.vals) > 0 {
				got = res.vals[0].(string)
			}
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
