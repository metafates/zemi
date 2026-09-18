package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	probeArg = "probe"
	// probeEnv stops a binary that does not call DispatchHelper (another
	// package's test binary) from probing again when run as the probe
	probeEnv = "ZEMI_SANDBOX_PROBE"

	// Landlock denies with EACCES, seccomp with EPERM; the namespaces give
	// read-only mounts (EBUSY to rename one) and a network with no route.
	// musl words the last two without "device or" and "is".
	deniedErrnos  = `operation not permitted|permission denied|read-only file system|(?:device or )?resource busy|network (?:is )?unreachable`
	deniedLookups = `temporary failure in name resolution`
)

// namespaces is whether wraps use the user+net+mount namespace variant, and
// pidns whether it gets a pid namespace too; set by probe.
var namespaces, pidns bool

func probe() (Backend, error) {
	exe, err := os.Executable()
	if err != nil {
		return Backend{}, err
	}
	if _, ok := seccompArch[runtime.GOARCH]; !ok {
		return Backend{}, fmt.Errorf("no seccomp filter for %s", runtime.GOARCH)
	}
	abi, err := landlockABI()
	if err != nil {
		return Backend{}, err
	}
	// before v3 Landlock does not handle truncate(2), so a command could
	// empty any of the user's files
	if abi < 3 {
		return Backend{}, fmt.Errorf("landlock v%d cannot deny truncate; needs linux 6.2 or newer", abi)
	}
	// each try confines a helper the way a wrap would, so an environment
	// that refuses landlock_restrict_self or seccomp() reports no backend
	if os.Getenv(probeEnv) == "" {
		try := func(mode string, pid bool) error {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, helperArg, probeArg, mode)
			cmd.Env = append(os.Environ(), probeEnv+"=1")
			cmd.SysProcAttr = &syscall.SysProcAttr{}
			if mode == "namespaces" {
				namespaceAttr(cmd.SysProcAttr, pid, false)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
			}
			return nil
		}
		pidns = try("namespaces", true) == nil
		namespaces = pidns || try("namespaces", false) == nil
		if !namespaces {
			if err := try("seccomp", false); err != nil {
				return Backend{}, fmt.Errorf("sandbox helper: %w", err)
			}
		}
	}
	if namespaces {
		return Backend{Name: fmt.Sprintf("landlock v%d + namespaces", abi), EnforcesProtect: true}, nil
	}
	return Backend{Name: fmt.Sprintf("landlock v%d + seccomp", abi)}, nil
}

// namespaceAttr starts the helper in its own user, mount, IPC, network
// (unless network, which keeps zemi's) and, with pid, pid namespaces, as the same uid and with the caps to bring lo up
// and mount. In its own pid namespace the command cannot name a process
// outside to signal it, which Landlock scopes only from v6.
// Docker's default seccomp profile and Ubuntu's AppArmor userns restriction
// both refuse this, and the probe falls back. Docker with seccomp unconfined
// masks parts of /proc, so a fresh proc for the pid namespace is refused
// there and the probe drops the pid namespace.
func namespaceAttr(a *syscall.SysProcAttr, pid, network bool) {
	a.Cloneflags |= syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC
	if !network {
		// its own empty network namespace; with the network it keeps zemi's,
		// where its caps (the user namespace owns none of it) buy nothing
		a.Cloneflags |= syscall.CLONE_NEWNET
	}
	if pid {
		a.Cloneflags |= syscall.CLONE_NEWPID
	}
	a.UidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getuid(), HostID: os.Getuid(), Size: 1}}
	a.GidMappings = []syscall.SysProcIDMap{{ContainerID: os.Getgid(), HostID: os.Getgid(), Size: 1}}
	a.AmbientCaps = []uintptr{unix.CAP_NET_ADMIN, unix.CAP_SYS_ADMIN, unix.CAP_SETPCAP}
}

// wrap runs cmd as `zemi __sandbox <mode> <policy> -- <path> <argv...>`,
// which confines itself and then execs the command in place (or, in a pid
// namespace, starts it as a child in the same session and group).
// NOTE: without namespaces Protect and Loopback are not enforced: Landlock
// cannot keep .git read-only inside a writable root without splitting the
// root, which breaks creating top-level files, and seccomp blocks every socket.
func wrap(cmd *exec.Cmd, p Policy) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	pol, err := json.Marshal(p)
	if err != nil {
		return err
	}
	mode := "seccomp"
	if namespaces {
		mode = "namespaces"
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		namespaceAttr(cmd.SysProcAttr, pidns, p.Network)
	}
	cmd.Args = append([]string{exe, helperArg, mode, string(pol), "--", cmd.Path}, cmd.Args...)
	cmd.Path = exe
	return nil
}

// Helper confines the process to the policy in args and runs the command.
// It must run before anything else in main: the confinement covers only
// the calling thread's exec.
// NOTE: an x86_64 command under Rosetta from an arm64 helper (a mixed-arch
// container) dies with SIGTRAP, exit 133: Rosetta cannot open /proc/self/exe.
func Helper(args []string) int {
	fail := func(step string, err error) int {
		fmt.Fprintf(os.Stderr, "zemi sandbox: %s: %v\n", step, err)
		return 125
	}
	runtime.LockOSThread()
	if len(args) == 2 && args[0] == probeArg {
		exe, err := os.Executable()
		if err != nil {
			return fail("probe", err)
		}
		ns := args[1] == "namespaces"
		p := Policy{Write: []string{"/"}}
		if ns {
			p.Protect, p.Loopback = []string{filepath.Dir(exe)}, true
		}
		if step, err := confine(p, ns); err != nil {
			return fail(step, err)
		}
		return 0
	}
	if len(args) < 5 || args[2] != "--" {
		return fail("args", errors.New("want <mode> <policy> -- <path> <argv...>"))
	}
	ns := args[0] == "namespaces"
	var p Policy
	if err := json.Unmarshal([]byte(args[1]), &p); err != nil {
		return fail("policy", err)
	}
	path, err := exec.LookPath(args[3])
	if err != nil {
		return fail("lookup", err)
	}
	if step, err := confine(p, ns); err != nil {
		return fail(step, err)
	}
	if ns && os.Getpid() == 1 {
		return runInit(path, args[4:], fail)
	}
	return fail("exec", syscall.Exec(path, args[4:], os.Environ()))
}

// confine applies p to the calling thread and names the step that failed.
func confine(p Policy, ns bool) (string, error) {
	if ns {
		if err := setupNamespaces(p); err != nil {
			return "namespaces", err
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return "no_new_privs", err
	}
	// root often builds a tree another uid owns (a bind-mounted checkout),
	// and Landlock still decides every path; in the user namespace such
	// files are unmapped, so the override would not apply there
	keep := uint64(1) << unix.CAP_DAC_OVERRIDE
	if ns {
		keep = 0
	}
	if err := dropCaps(keep); err != nil {
		return "capabilities", err
	}
	if err := landlock(p); err != nil {
		return "landlock", err
	}
	if err := seccomp(ns, p.Network); err != nil {
		return "seccomp", err
	}
	return "", nil
}

// dropCaps empties every capability set but keep, the bounding set too
// when it may: root execs with its bounding set, which in the user
// namespace holds CAP_SYS_ADMIN, enough for mount_setattr to make a
// protected bind mount writable again. Clearing inheritable clears ambient.
func dropCaps(keep uint64) error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return err
	}
	// a non-root helper has no CAP_SETPCAP to shrink the bounding set, and
	// under no_new_privs its exec gains nothing from it
	if data[0].Effective&(1<<unix.CAP_SETPCAP) != 0 {
		for c := uintptr(0); ; c++ {
			if keep&(1<<c) != 0 {
				continue
			}
			err := unix.Prctl(unix.PR_CAPBSET_DROP, c, 0, 0, 0)
			if errors.Is(err, unix.EINVAL) { // past the last cap
				break
			}
			if err != nil {
				return fmt.Errorf("bounding set: %w", err)
			}
		}
	}
	for i := range data {
		k := uint32(keep >> (32 * i))
		data[i] = unix.CapUserData{Effective: data[i].Effective & k, Permitted: data[i].Permitted & k}
	}
	return unix.Capset(&hdr, &data[0])
}

// runInit is pid 1 of the command's pid namespace: it starts the command,
// reaps what gets orphaned to it and exits with the command's status. The
// command shares its process group, so zemi's group kill and hangup and a
// terminal's ^C reach it directly; init swallows its own copy, or the Go
// runtime would exit on it and take the namespace down first.
// NOTE: a command killed by a signal exits 128+signal, not signaled: pid 1
// cannot deliver itself a fatal signal.
func runInit(path string, argv []string, fail func(string, error) int) int {
	signal.Notify(make(chan os.Signal, 1))
	pid, err := syscall.ForkExec(path, argv, &syscall.ProcAttr{Env: os.Environ(), Files: []uintptr{0, 1, 2}})
	if err != nil {
		return fail("exec", err)
	}
	for {
		var ws syscall.WaitStatus
		wpid, err := syscall.Wait4(-1, &ws, 0, nil)
		switch {
		case errors.Is(err, syscall.EINTR), err == nil && wpid != pid:
		case err != nil:
			return fail("wait", err)
		case ws.Signaled():
			return 128 + int(ws.Signal())
		default:
			return ws.ExitStatus()
		}
	}
}

// setupNamespaces brings lo up for Loopback, mounts a proc for a pid
// namespace, and makes each Protect path
// under a Write root a read-only bind mount (and a Write root inside one
// writable again). Landlock forbids the command to umount them, and with
// no caps left it cannot remount them.
func setupNamespaces(p Policy) error {
	if p.Loopback && !p.Network { // shared with zemi: lo is already up
		if err := loUp(); err != nil {
			return err
		}
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("private mounts: %w", err)
	}
	// the outer /proc lists the outer pids, not the ones the command sees
	if os.Getpid() == 1 {
		if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("/proc: %w", err)
		}
	}
	if _, err := os.Stat("/dev/shm"); err == nil {
		if err := unix.Mount("tmpfs", "/dev/shm", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
			return fmt.Errorf("/dev/shm: %w", err)
		}
	}
	type bind struct {
		path     string
		readOnly bool
	}
	var binds []bind
	inWrite := func(path string) bool {
		return slices.ContainsFunc(p.Write, func(w string) bool { return within(path, w) })
	}
	for _, pr := range p.Protect {
		if inWrite(pr) {
			binds = append(binds, bind{pr, true})
		}
	}
	for _, w := range p.Write {
		if slices.ContainsFunc(p.Protect, func(pr string) bool { return w != pr && within(w, pr) }) {
			binds = append(binds, bind{w, false})
		}
	}
	// deeper mounts go on top
	slices.SortStableFunc(binds, func(a, b bind) int { return len(a.path) - len(b.path) })
	for _, b := range binds {
		// NOTE: a mount follows a symlink, so a protected link itself stays
		// removable (Wrap protects its target too); only a read-only parent
		// would keep it.
		if fi, err := os.Lstat(b.path); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if err := unix.Mount(b.path, b.path, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s: %w", b.path, err)
		}
		// a remount must keep the flags the outer namespace locked
		var st unix.Statfs_t
		if err := unix.Statfs(b.path, &st); err != nil {
			return err
		}
		flags := unix.MS_REMOUNT | unix.MS_BIND | uintptr(st.Flags)&(unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_NOATIME|unix.MS_NODIRATIME|unix.MS_RELATIME)
		if b.readOnly {
			flags |= unix.MS_RDONLY
		}
		if err := unix.Mount("", b.path, "", flags, ""); err != nil {
			return fmt.Errorf("remount %s: %w", b.path, err)
		}
	}
	return nil
}

func loUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("lo: %w", err)
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("lo up: %w", err)
	}
	return nil
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
	attr := unix.LandlockRulesetAttr{Access_fs: 1<<13 - 1 | unix.LANDLOCK_ACCESS_FS_REFER | unix.LANDLOCK_ACCESS_FS_TRUNCATE}
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
	// other terminals and input devices: reading one reads what the user
	// types there. /dev/tty and a pty fd the command inherits still work,
	// since Landlock checks at open.
	// NOTE: so /dev/stdin in a window, which reopens its pty, fails too
	devices := []string{"/dev/pts", "/dev/console", "/dev/input"}
	for _, g := range []string{"/dev/tty?*", "/dev/vcs*", "/dev/hidraw*"} {
		m, _ := filepath.Glob(g)
		devices = append(devices, m...)
	}
	var hide []string
	for _, h := range append(p.Hide, devices...) {
		if _, err := os.Lstat(h); err == nil {
			hide = append(hide, h)
		}
	}
	if err := addRead(add, "/", hide); err != nil {
		return err
	}
	// NOTE: without namespaces /dev/shm is the one the user's other programs
	// share, so their POSIX shared memory is writable too.
	for _, w := range append(p.Write, "/dev/null", "/dev/zero", "/dev/tty", "/dev/ptmx", "/dev/shm") {
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

// seccomp denies io_uring (whose ops skip the socket check), chown and
// xattrs (ACLs among them) and kernel keyrings, which Landlock does not
// mediate, SysV IPC without the IPC namespace, and socket():
// every family without namespaces, and with them all but inet and netlink,
// which reach only the empty network namespace. A policy with Network
// allows those two families here as well, and there the namespace is zemi's
// own: the inet socket is the point. Landlock does not mediate
// connect() on a pathname unix socket (docker.sock, $SSH_AUTH_SOCK, the
// systemd user bus), so AF_UNIX is denied in both.
// NOTE: kill is not filtered, so before Landlock v6 scopes signals the
// seccomp variant can signal any process of the user, zemi and an editor
// with unsaved work included. Requiring v6 (linux 6.12) would turn the
// sandbox off on the LTS kernels most machines run; a signal cannot read
// or write a file.
// NOTE: chmod and utimensat stay allowed, since builds set modes and times
// and seccomp cannot see the path: a command can change the mode of any of
// the user's files (chmod 644 ~/.ssh/id_ed25519), though not read or write
// it. The namespace variant cannot remount home read-only for metadata alone
// either; the upgrade is a Landlock right for it, which no kernel has yet.
// NOTE: without namespaces SysV IPC is denied outright, so a program that
// needs it (PostgreSQL's postmaster) fails there. /proc/sysvipc still lists
// the user's segments (key, owner, size), and POSIX message queues stay
// shared with the user's other programs.
// NOTE: socketpair stays allowed, so a unix socket still works between the
// command's own processes; anything that listens on or dials a unix socket
// path fails, and an AF_UNIX fd the command inherits stays usable.
func seccomp(ns, network bool) error {
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
	deny, allow := stmt(ret, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)), stmt(ret, unix.SECCOMP_RET_ALLOW)
	// each test sits right before the return it jumps into or over
	if runtime.GOARCH == "amd64" {
		// x32 syscalls share the arch but not the numbers
		prog = append(prog, jump(unix.BPF_JMP|unix.BPF_JGE|unix.BPF_K, 0x40000000, 0, 1), deny)
	}
	metadata := []uint32{unix.SYS_FCHOWN, unix.SYS_FCHOWNAT, unix.SYS_SETXATTR, unix.SYS_LSETXATTR, unix.SYS_FSETXATTR,
		unix.SYS_SETXATTRAT, unix.SYS_REMOVEXATTR, unix.SYS_LREMOVEXATTR, unix.SYS_FREMOVEXATTR, unix.SYS_REMOVEXATTRAT, unix.SYS_FILE_SETATTR}
	if runtime.GOARCH == "amd64" {
		metadata = append(metadata, 92, 94) // chown, lchown; arm64 has only the at forms
	}
	denied := append(metadata, unix.SYS_IO_URING_SETUP, unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY)
	if !ns {
		denied = append(denied, unix.SYS_SHMGET, unix.SYS_SHMAT, unix.SYS_SHMCTL, unix.SYS_MSGGET, unix.SYS_MSGSND,
			unix.SYS_MSGRCV, unix.SYS_MSGCTL, unix.SYS_SEMGET, unix.SYS_SEMOP, unix.SYS_SEMTIMEDOP, unix.SYS_SEMCTL)
	}
	for _, nr := range denied {
		prog = append(prog, jump(eq, nr, 0, 1), deny)
	}
	prog = append(prog, jump(eq, unix.SYS_SOCKET, 1, 0), allow)
	if ns || network {
		prog = append(prog, stmt(load, offArg))
		for _, family := range []uint32{unix.AF_INET, unix.AF_INET6, unix.AF_NETLINK} {
			prog = append(prog, jump(eq, family, 0, 1), allow)
		}
	}
	prog = append(prog, deny)
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return errno
	}
	return nil
}
