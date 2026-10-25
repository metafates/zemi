// The TUI owns the terminal: while the altscreen is up, anything the process
// writes to stderr - a panic's stack, a chatty library - would land on the
// screen, and the renderer repaints only its own changes, so it would stay
// there. These files put fd 2 into the launch log instead.

//go:build windows

package cli

import (
	"os"
	"sync"
)

// stderrToLog sends what the process writes through os.Stderr to the launch
// log. Windows has no fd 2 to swap, so the runtime's own crash output keeps
// going to the console; library writes through os.Stderr are covered.
func stderrToLog(path string) (restore func(), ok bool) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return func() {}, false
	}
	old := os.Stderr
	os.Stderr = f
	return sync.OnceFunc(func() {
		os.Stderr = old
		f.Close()
	}), true
}
