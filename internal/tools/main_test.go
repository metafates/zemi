package tools

import (
	"testing"

	"github.com/metafates/zemi/internal/sandbox"
)

// On Linux the sandbox probe and Wrap re-exec this binary as the helper.
func TestMain(m *testing.M) {
	sandbox.DispatchHelper()
	m.Run()
}
