package ui

import (
	"os"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/sandbox"
)

// On Linux the sandbox probe and Wrap re-exec this binary as the helper.
func TestMain(m *testing.M) {
	sandbox.DispatchHelper()
	time.Local = time.UTC // a turn's end time in a golden reads the same everywhere
	os.Exit(m.Run())
}
