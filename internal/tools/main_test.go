package tools

import (
	"os"
	"testing"

	"github.com/metafates/zemi/internal/sandbox"
)

// On Linux the sandbox probe and Wrap re-exec this binary as the helper.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "__sandbox" {
		os.Exit(sandbox.Helper(os.Args[2:]))
	}
	os.Exit(m.Run())
}
