package hostapi

import (
	"os"
	"testing"

	"github.com/metafates/zemi/internal/lsp/lsptest"
)

// The test binary doubles as a fake language server for the LSP tests.
func TestMain(m *testing.M) {
	// the LSP tests start this binary with the env set, as a fake server
	if os.Getenv("ZEMI_LSP_FAKE") == "1" {
		lsptest.Serve(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	m.Run()
}
