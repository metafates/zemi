//go:build unix

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStderrToLog(t *testing.T) {
	var before unix.Stat_t
	if err := unix.Fstat(2, &before); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "launch.log")
	restore, ok := stderrToLog(path)
	if !ok {
		t.Fatal("redirect failed")
	}
	t.Cleanup(restore)

	fmt.Fprintln(os.Stderr, "stderr lands in the log")
	got, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(got), "stderr lands in the log") {
		t.Fatalf("stderr write did not land in the log: %q (%v)", got, err)
	}

	restore()
	var after unix.Stat_t
	if err := unix.Fstat(2, &after); err != nil {
		t.Fatal(err)
	}
	if before.Dev != after.Dev || before.Ino != after.Ino {
		t.Fatal("restore did not put the original stderr back on fd 2")
	}
}
