package sandbox

// NOTE: v1 is Landlock + seccomp, which runs even under Docker's default
// seccomp profile. The upgrade path is a user+net+mount namespace variant:
// loopback that works, and Protect as read-only bind mounts.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const helperArg = "__sandbox"

func probe() (Backend, error) {
	if _, err := os.Executable(); err != nil {
		return Backend{}, err
	}
	if _, ok := seccompArch[runtime.GOARCH]; !ok {
		return Backend{}, fmt.Errorf("no seccomp filter for %s", runtime.GOARCH)
	}
	abi, err := landlockABI()
	if err != nil {
		return Backend{}, err
	}
	return Backend{Name: fmt.Sprintf("landlock v%d + seccomp", abi)}, nil
}

// wrap runs cmd as `zemi __sandbox <policy> -- <path> <argv...>`, which
// confines itself and then execs the command in place.
// NOTE: the Policy's Protect and Loopback are not enforced here. Landlock
// cannot keep .git read-only inside a writable root without splitting the
// root, which breaks creating top-level files; seccomp blocks every inet socket.
func wrap(cmd *exec.Cmd, p Policy) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	pol, err := json.Marshal(p)
	if err != nil {
		return err
	}
	cmd.Args = append([]string{exe, helperArg, string(pol), "--", cmd.Path}, cmd.Args...)
	cmd.Path = exe
	return nil
}

// Helper confines the process to the policy in args and execs the command.
// It must run before anything else in main: the confinement covers only
// the calling thread's exec.
func Helper(args []string) int {
	fail := func(step string, err error) int {
		fmt.Fprintf(os.Stderr, "zemi sandbox: %s: %v\n", step, err)
		return 125
	}
	if len(args) < 4 || args[1] != "--" {
		return fail("args", errors.New("want <policy> -- <path> <argv...>"))
	}
	var p Policy
	if err := json.Unmarshal([]byte(args[0]), &p); err != nil {
		return fail("policy", err)
	}
	path, err := exec.LookPath(args[2])
	if err != nil {
		return fail("lookup", err)
	}
	runtime.LockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fail("no_new_privs", err)
	}
	if err := landlock(p); err != nil {
		return fail("landlock", err)
	}
	if err := seccomp(); err != nil {
		return fail("seccomp", err)
	}
	return fail("exec", syscall.Exec(path, args[3:], os.Environ()))
}

const (
	fsReadRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	// the rights Landlock accepts on a rule for a non-directory
	fsFileRights = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
)

func landlockABI() (int, error) {
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, fmt.Errorf("landlock unavailable: %w", errno)
	}
	return int(abi), nil
}

func landlock(p Policy) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}
	attr := unix.LandlockRulesetAttr{Access_fs: 1<<13 - 1}
	if abi >= 2 {
		attr.Access_fs |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		attr.Access_fs |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		attr.Access_fs |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	if abi >= 6 {
		attr.Scoped = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("create ruleset: %w", errno)
	}
	defer unix.Close(int(fd))
	add := func(path string, access uint64) error {
		pfd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer unix.Close(pfd)
		var st unix.Stat_t
		if err := unix.Fstat(pfd, &st); err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			access &= fsFileRights
		}
		rule := unix.LandlockPathBeneathAttr{Allowed_access: access & attr.Access_fs, Parent_fd: int32(pfd)}
		if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
			return fmt.Errorf("add rule %s: %w", path, errno)
		}
		return nil
	}
	var hide []string
	for _, h := range p.Hide {
		if _, err := os.Lstat(h); err == nil {
			hide = append(hide, h)
		}
	}
	if err := addRead(add, "/", hide); err != nil {
		return err
	}
	for _, w := range append(p.Write, "/dev/null", "/dev/zero", "/dev/tty", "/dev/pts", "/dev/ptmx") {
		if err := add(w, attr.Access_fs); err != nil {
			return err
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("restrict self: %w", errno)
	}
	return nil
}

// addRead grants read below dir except under hide. A rule covers its whole
// subtree, so an ancestor of a hidden path gets READ_DIR alone and each of
// its other entries a rule of its own.
// NOTE: READ_DIR on the ancestor reaches the hidden dir too, so its file
// names stay listable; only the contents are unreadable.
func addRead(add func(string, uint64) error, dir string, hide []string) error {
	if slices.Contains(hide, dir) {
		return nil
	}
	if !slices.ContainsFunc(hide, func(h string) bool { return within(h, dir) }) {
		return add(dir, fsReadRights)
	}
	if err := add(dir, unix.LANDLOCK_ACCESS_FS_READ_DIR); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		// symlinks are reached through their targets' own rules
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if err := addRead(add, filepath.Join(dir, e.Name()), hide); err != nil {
			return err
		}
	}
	return nil
}

var seccompArch = map[string]uint32{"amd64": unix.AUDIT_ARCH_X86_64, "arm64": unix.AUDIT_ARCH_AARCH64}

// seccomp denies inet sockets and io_uring (whose ops skip the socket check).
// NOTE: pathname unix sockets (docker.sock, $SSH_AUTH_SOCK) stay reachable;
// masking them needs the mount namespace variant.
func seccomp() error {
	arch, ok := seccompArch[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("no filter for %s", runtime.GOARCH)
	}
	stmt := func(code uint16, k uint32) unix.SockFilter { return unix.SockFilter{Code: code, K: k} }
	jump := func(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
		return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
	}
	const (
		load   = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		eq     = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		ret    = unix.BPF_RET | unix.BPF_K
		offNr  = 0
		offArc = 4
		offArg = 16 // low half of args[0] on little-endian amd64 and arm64
	)
	prog := []unix.SockFilter{
		stmt(load, offArc),
		jump(eq, arch, 1, 0),
		stmt(ret, unix.SECCOMP_RET_KILL_PROCESS),
		stmt(load, offNr),
	}
	if runtime.GOARCH == "amd64" {
		// x32 syscalls share the arch but not the numbers
		prog = append(prog, jump(unix.BPF_JMP|unix.BPF_JGE|unix.BPF_K, 0x40000000, 4, 0))
	}
	prog = append(prog,
		jump(eq, unix.SYS_IO_URING_SETUP, 3, 0),
		jump(eq, unix.SYS_SOCKET, 0, 3),
		stmt(load, offArg),
		jump(eq, unix.AF_UNIX, 1, 0),
		stmt(ret, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)),
		stmt(ret, unix.SECCOMP_RET_ALLOW),
	)
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return errno
	}
	return nil
}
