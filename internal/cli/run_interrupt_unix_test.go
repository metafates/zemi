//go:build unix

package cli

import (
	"os"
	"runtime"
	"syscall"
	"testing"
)

// TestRunInterruptJSON: an interrupted run, /btw included, still ends in its
// done line, so a script keeps the session id it resumes; the signal sets
// the exit status, 128+n.
func TestRunInterruptJSON(t *testing.T) {
	for _, c := range []struct {
		prompt string
		sig    syscall.Signal
		code   int
	}{
		{"hang", syscall.SIGINT, 130},
		{"/btw hang", syscall.SIGINT, 130},
		{"hang", syscall.SIGTERM, 143},
	} {
		h := printHost(t)
		if code, _, _ := runPrompt(t, h, options{prompt: "say hi", quiet: true}); code != 0 {
			t.Fatal("first turn failed")
		}
		type result struct {
			code int
			out  string
		}
		done := make(chan result, 1)
		go func() {
			code, out, _ := runPrompt(t, h, options{prompt: c.prompt, json: true})
			done <- result{code, out}
		}()
		for !h.AgentActive() {
			select {
			case r := <-done:
				t.Fatalf("%s: ended with %d before the interrupt", c.prompt, r.code)
			default:
				runtime.Gosched()
			}
		}
		if err := syscall.Kill(os.Getpid(), c.sig); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if d := lastDone(t, r.out); r.code != c.code || !d.IsError || d.Error != "interrupted" || d.Session == "" || h.AgentActive() {
			t.Errorf("%s: code %d, stdout %q, still active %v", c.prompt, r.code, r.out, h.AgentActive())
		}
	}
}
