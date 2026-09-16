package hostapi

import (
	"os"
	"testing"

	"github.com/metafates/zemi/internal/lsp/lsptest"
	"github.com/metafates/zemi/internal/sandbox"
)

// The test binary doubles as a fake language server for the LSP tests.
func TestMain(m *testing.M) {
	// on Linux the sandbox probe and Wrap re-exec this binary as the helper
	if len(os.Args) > 2 && os.Args[1] == "__sandbox" {
		os.Exit(sandbox.Helper(os.Args[2:]))
	}
	if os.Getenv("ZEMI_LSP_FAKE") == "1" {
		lsptest.Serve(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
