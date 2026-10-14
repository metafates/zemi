//go:build windows

package cli

import "testing"

func limitFDs(t *testing.T, _ uint64) func() {
	t.Skip("no rlimit on windows")
	return nil
}
