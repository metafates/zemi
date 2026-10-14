//go:build windows

package skill

import "testing"

func makeFifo(t *testing.T, _ string) { t.Skip("no mkfifo on windows") }
