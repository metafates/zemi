package hostapi

import (
	"flag"
	"os"
	"testing"

	"go.uber.org/goleak"

	"github.com/metafates/zemi/internal/lsp/lsptest"
)

// The test binary doubles as a fake language server for the LSP tests.
func TestMain(m *testing.M) {
	// the LSP tests start this binary with the env set, as a fake server
	if os.Getenv("ZEMI_LSP_FAKE") == "1" {
		lsptest.Serve(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	// parallel host boots saturate the kernel past four, and the veto and
	// turn ceilings measure wall time however loaded the machine is
	flag.Set("test.parallel", "4")
	// regexp2's lexer clock sleeps out its last idle second on its own
	goleak.VerifyTestMain(m, goleak.IgnoreAnyFunction("github.com/dlclark/regexp2/v2.runClock"))
}
