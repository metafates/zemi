package sandbox

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func init() {
	osActs["keyctl"] = func(string) error {
		_, err := unix.KeyctlGetKeyringID(unix.KEY_SPEC_USER_KEYRING, true)
		return wrapErr("keyctl", err)
	}
	osActs["add_key"] = func(string) error {
		_, err := unix.AddKey("user", "zemi", []byte("x"), unix.KEY_SPEC_PROCESS_KEYRING)
		return wrapErr("add_key", err)
	}
	osActs["shmget"] = func(key string) error {
		k, _ := strconv.Atoi(key)
		_, err := unix.SysvShmGet(k, 0, 0)
		return wrapErr("shmget", err)
	}
}

func wrapErr(call string, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", call, err)
	}
	return nil
}

// Keyrings and SysV IPC are kernel state Landlock does not see.
func TestKernelState(t *testing.T) {
	probeOrSkip(t)
	key := 0x5a3e0000 + os.Getpid()&0xffff
	id, err := unix.SysvShmGet(key, 4096, unix.IPC_CREAT|0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.SysvShmCtl(id, unix.IPC_RMID, nil)
	for _, c := range []struct{ verb, arg, want string }{
		{"keyctl", "", "operation not permitted"},
		{"add_key", "", "operation not permitted"},
		// the namespace variant has its own, empty IPC namespace
		{"shmget", strconv.Itoa(key), "operation not permitted|no such file"},
	} {
		t.Run(c.verb, func(t *testing.T) {
			out, err := run(t, Policy{}, c.verb, c.arg)
			if err == nil || !regexp.MustCompile(c.want).MatchString(out) {
				t.Fatalf("not confined: %v\n%s", err, out)
			}
		})
	}
}
