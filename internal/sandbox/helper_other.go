//go:build !linux

package sandbox

import (
	"fmt"
	"os"
)

// helper is the re-exec entry only the Linux backend uses.
func helper([]string) int {
	fmt.Fprintln(os.Stderr, "zemi sandbox: the helper is linux only")
	return 2
}
