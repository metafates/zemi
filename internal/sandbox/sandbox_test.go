//go:build darwin || linux

package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// The test binary doubles as the sandboxed program (__act) and, on Linux,
// as the helper zemi's main dispatches to.
func TestMain(m *testing.M) {
	DispatchHelper()
	if len(os.Args) > 1 && os.Args[1] == "__act" {
		if err := act(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// osActs are the verbs only one OS's test file can build.
var osActs = map[string]func(arg string) error{}

func act(verb, arg string) error {
	if f, ok := osActs[verb]; ok {
		return f(arg)
	}
	switch verb {
	case "write":
		return os.WriteFile(arg, []byte("x"), 0o644)
	case "mkwrite":
		if err := os.MkdirAll(filepath.Dir(arg), 0o755); err != nil {
			return err
		}
		return os.WriteFile(arg, []byte("x"), 0o644)
	case "read":
		_, err := os.ReadFile(arg)
		return err
	case "stat":
		_, err := os.Stat(arg)
		return err
	case "ls":
		_, err := os.ReadDir(arg)
		return err
	case "realpath":
		if _, err := filepath.EvalSymlinks(arg); err != nil {
			return err
		}
		// realpath(3), which cargo's canonicalize calls
		if out, err := exec.Command("realpath", arg).CombinedOutput(); err != nil {
			return fmt.Errorf("realpath: %w: %s", err, out)
		}
	case "env":
		if v := os.Getenv("ZEMI_SANDBOX_TEST"); v != arg {
			return fmt.Errorf("env %q", v)
		}
	case "open":
		f, err := os.Open(arg)
		if err == nil {
			f.Close()
		}
		return err
	case "status":
		b, err := os.ReadFile("/proc/self/status")
		fmt.Print(string(b))
		return err
	case "rename":
		return os.Rename(arg, arg+".moved")
	case "dial":
		c, err := net.DialTimeout("tcp", arg, 3*time.Second)
		if err == nil {
			c.Close()
		}
		return err
	case "https":
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(arg)
		if err == nil {
			resp.Body.Close()
		}
		return err
	case "unix":
		c, err := net.DialTimeout("unix", arg, 3*time.Second)
		if err == nil {
			c.Close()
		}
		return err
	case "loopback":
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		defer l.Close()
		c, err := net.Dial("tcp", l.Addr().String())
		if err == nil {
			c.Close()
		}
		return err
	case "chmod":
		return os.Chmod(arg, 0o600)
	case "chown":
		return os.Chown(arg, os.Getuid(), os.Getgid())
	case "setxattr":
		if err := unix.Setxattr(arg, "user.zemi", []byte("x"), 0); err != nil {
			return fmt.Errorf("setxattr %s: %w", arg, err)
		}
	case "procpid":
		b, err := os.ReadFile("/proc/self/stat")
		if err != nil {
			return err
		}
		if pid, _, _ := strings.Cut(string(b), " "); pid != strconv.Itoa(os.Getpid()) {
			return fmt.Errorf("/proc/self is %s, getpid %d", pid, os.Getpid())
		}
	case "pid":
		fmt.Println(os.Getpid())
		time.Sleep(10 * time.Second)
	}
	return nil
}

func run(t *testing.T, p Policy, verb, arg string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "__act", verb, arg)
	if err := Wrap(cmd, p); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func probeOrSkip(t *testing.T) Backend {
	b, err := Probe()
	if err != nil {
		t.Skip("no sandbox:", err)
	}
	return b
}

func TestPolicy(t *testing.T) {
	b := probeOrSkip(t)
	t.Log("backend:", b.Name)
	root, outside, hide, state := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	scratch, cache := filepath.Join(state, "scratch"), filepath.Join(state, "sandbox-cache", "k", "go-build")
	for _, d := range []string{filepath.Join(root, ".git"), filepath.Join(root, "zemi-real"), scratch, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(root, "plain.txt"), filepath.Join(root, ".env"), filepath.Join(hide, "id"), filepath.Join(state, "zemi.db")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("zemi-real", filepath.Join(root, ".zemi")); err != nil {
		t.Fatal(err)
	}
	p := Policy{
		Write:    []string{root, scratch, cache},
		Protect:  []string{filepath.Join(root, ".git"), filepath.Join(root, ".zemi"), state},
		Hide:     []string{hide, state},
		Loopback: true,
		Env:      map[string]string{"ZEMI_SANDBOX_TEST": "on"},
	}
	_, caseFolds := os.Stat(filepath.Join(root, ".GIT"))
	// a short dir: socket paths are capped near 100 bytes
	sockDir, err := os.MkdirTemp("/tmp", "zs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock, err := net.Listen("unix", filepath.Join(sockDir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	shm := fmt.Sprintf("/dev/shm/zemi-sandbox-%d", os.Getpid())
	t.Cleanup(func() { os.Remove(shm) })
	cases := []struct {
		name, verb, arg string
		allowed, skip   bool
	}{
		{"write inside a root", "write", filepath.Join(root, "a.txt"), true, false},
		{"read a plain file", "read", filepath.Join(root, "plain.txt"), true, false},
		{"policy env reaches the command", "env", "on", true, false},
		{"write in a root under a protected dir", "write", filepath.Join(scratch, "a.txt"), true, false},
		{"write outside the roots", "write", filepath.Join(outside, "a.txt"), false, false},
		{"read back in a root under a hidden dir", "read", filepath.Join(scratch, "a.txt"), true, false},
		{"write a protected dir", "write", filepath.Join(state, "a.txt"), false, false},
		{"read a hidden dir holding a root", "read", filepath.Join(state, "zemi.db"), false, false},
		{"stat a hidden dir holding a root", "stat", state, true, false},
		{"list a hidden dir holding a root", "ls", state, false, false},
		{"list a hidden parent of a deep root", "ls", filepath.Join(state, "sandbox-cache"), false, false},
		{"realpath a root deep in a hidden dir", "realpath", cache, true, false},
		{"write in a root deep in a hidden dir", "write", filepath.Join(cache, "a"), true, false},
		{"write .git/config", "write", filepath.Join(root, ".git", "config"), false, !b.EnforcesProtect},
		{"write .GIT/config", "write", filepath.Join(root, ".GIT", "config"), false, !b.EnforcesProtect || caseFolds != nil},
		{"create a nested .git", "mkwrite", filepath.Join(root, "pkg", ".git", "config"), false, runtime.GOOS == "linux"},
		{"create a nested .Zemi", "mkwrite", filepath.Join(root, "pkg", ".Zemi", "init.lua"), false, runtime.GOOS == "linux"},
		{"write .gitignore", "write", filepath.Join(root, ".gitignore"), true, false},
		{"write .github", "mkwrite", filepath.Join(root, ".github", "x.yml"), true, false},
		{"create a .git in a root without protected paths", "mkwrite", filepath.Join(scratch, "repo", ".git", "config"), true, false},
		{"rename .git", "rename", filepath.Join(root, ".git"), false, !b.EnforcesProtect},
		{"write through a protected symlink", "write", filepath.Join(root, ".zemi", "init.lua"), false, !b.EnforcesProtect},
		// NOTE: see setupNamespaces
		{"rename a protected symlink", "rename", filepath.Join(root, ".zemi"), false, !b.EnforcesProtect || runtime.GOOS == "linux"},
		{"read a hidden dir", "read", filepath.Join(hide, "id"), false, false},
		{"read .env", "read", filepath.Join(root, ".env"), false, !b.HidesSecretFiles},
		{"dial a unix socket outside", "unix", sock.Addr().String(), false, false},
		{"write /dev/shm", "write", shm, true, runtime.GOOS != "linux"},
		{"chmod a file in a root", "chmod", filepath.Join(root, "plain.txt"), true, false},
		// Landlock has no right for these, see seccomp
		{"chown a file in a root", "chown", filepath.Join(root, "plain.txt"), false, runtime.GOOS != "linux"},
		{"set an xattr in a root", "setxattr", filepath.Join(root, "plain.txt"), false, runtime.GOOS != "linux"},
		{"/proc agrees with getpid", "procpid", "", true, runtime.GOOS != "linux"},
		// the namespaces that keep Protect read-only also give a loopback
		{"listen and dial loopback", "loopback", "", true, !b.EnforcesProtect},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.skip {
				t.Skipf("%s does not enforce this", b.Name)
			}
			out, err := run(t, p, c.verb, c.arg)
			if c.allowed && err != nil {
				t.Fatalf("denied: %v\n%s", err, out)
			}
			if !c.allowed && (err == nil || !LikelyDenied(out, err)) {
				t.Fatalf("not denied: %v\n%s", err, out)
			}
		})
	}
}

func TestNetworkDenied(t *testing.T) {
	probeOrSkip(t)
	dial := func(addr string, loopback bool) {
		t.Helper()
		out, err := run(t, Policy{Loopback: loopback}, "dial", addr)
		// refused at once, not a timeout: the connect never left the machine
		if err == nil || !LikelyDenied(out, err) {
			t.Fatalf("dial %s: %v\n%s", addr, err, out)
		}
	}
	dial("192.0.2.1:9", true)
	lo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lo.Close()
	dial(lo.Addr().String(), false)
	ip := nonLoopbackIP()
	if ip == nil {
		return
	}
	l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		return
	}
	defer l.Close()
	go func() {
		for c, err := l.Accept(); err == nil; c, err = l.Accept() {
			c.Close()
		}
	}()
	// seatbelt's localhost covers this machine's own addresses, so only
	// without Loopback
	dial(l.Addr().String(), false)
}

// TestNetworkGranted: the middle tier hands the command IP sockets and keeps
// every file rule, so "it needs the network" costs a socket, not the machine.
func TestNetworkGranted(t *testing.T) {
	probeOrSkip(t)
	ip := nonLoopbackIP()
	if ip == nil {
		t.Skip("no non-loopback address to dial")
	}
	l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	go func() {
		for c, err := l.Accept(); err == nil; c, err = l.Accept() {
			c.Close()
		}
	}()
	hidden := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(hidden, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// a short dir: socket paths are capped near 100 bytes
	sockDir, err := os.MkdirTemp("/tmp", "zs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock, err := net.Listen("unix", filepath.Join(sockDir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer sock.Close()
	const site = "https://example.com"
	offline := runtime.GOOS != "darwin" || act("https", site) != nil
	for _, c := range []struct {
		name, verb, arg string
		allowed, skip   bool
	}{
		{"dial another host", "dial", l.Addr().String(), true, false},
		{"read a hidden file", "read", hidden, false, false},
		{"dial a unix socket outside", "unix", sock.Addr().String(), false, false},
		{"read the system CA bundle", "read", "/etc/ssl/cert.pem", true, runtime.GOOS != "darwin"},
		{"verify TLS through the system", "https", site, true, offline},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.skip {
				t.Skip("not on this machine")
			}
			out, err := run(t, Policy{Network: true, Hide: []string{filepath.Dir(hidden)}}, c.verb, c.arg)
			if (err == nil) != c.allowed {
				t.Fatalf("allowed %v, got %v\n%s", c.allowed, err, out)
			}
		})
	}
}

func nonLoopbackIP() net.IP {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP
		}
	}
	return nil
}

// zemi stops a command by killing its process group, so the program must
// stay in the group the wrap started, and die with it.
func TestGroupKill(t *testing.T) {
	b := probeOrSkip(t)
	cmd := exec.Command(os.Args[0], "__act", "pid", "")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := Wrap(cmd, Policy{}); err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	group := []int{pid}
	if runtime.GOOS == "linux" {
		group = groupOf(pid)
	}
	// the namespace variant runs the program under an init of its own
	if inner := len(group) > 1; strings.TrimSpace(line) != fmt.Sprint(pid) && !(inner && b.EnforcesProtect) {
		t.Fatalf("program pid %s, started %d, group %v", line, pid, group)
	}
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Fatalf("pgid %d, %v; want %d", pgid, err, pid)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		left := slices.DeleteFunc(groupOf(pid), func(p int) bool { return p == pid })
		if runtime.GOOS != "linux" || len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running after the group kill: %v", left)
		}
	}
}

// groupOf lists the live, non-zombie processes in process group pgid.
func groupOf(pgid int) []int {
	var out []int
	dirs, _ := os.ReadDir("/proc")
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "stat"))
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp ...; comm may hold spaces
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
		if len(f) > 2 && f[0] != "Z" && f[2] == fmt.Sprint(pgid) {
			out = append(out, pid)
		}
	}
	return out
}

func TestCaps(t *testing.T) {
	b := probeOrSkip(t)
	if runtime.GOOS != "linux" {
		t.Skip("capabilities are linux only")
	}
	out, err := run(t, Policy{}, "status", "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(l, ":")
		n, _ := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
		switch {
		case !strings.HasPrefix(k, "Cap"), n == 0:
		// without CAP_SETPCAP a non-root helper cannot shrink the bounding
		// set, and no_new_privs keeps exec from raising caps to it
		case k == "CapBnd" && !b.EnforcesProtect && os.Getuid() != 0:
		// the seccomp variant keeps CAP_DAC_OVERRIDE (bit 1), see Helper
		case n == 1<<1 && !b.EnforcesProtect:
		default:
			t.Errorf("%s %016x", k, n)
		}
	}
}

func TestLikelyDenied(t *testing.T) {
	exit := func(code int) error {
		return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	}
	linux := runtime.GOOS == "linux"
	for _, c := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"success", "open x: operation not permitted", nil, false},
		{"go write denied", "open x: operation not permitted", exit(1), true},
		{"go cache write denied", "open /Users/u/Library/Caches/go-build/7f/7fe2-d: operation not permitted\nFAIL\te2e [setup failed]", exit(1), true},
		{"python denied", "PermissionError: [Errno 1] Operation not permitted: 'out.txt'", exit(1), true},
		{"make over a denied touch", "touch planmade\ntouch: planmade: Operation not permitted\nmake: *** [w] Error 1", exit(2), true},
		{"silent curl", "", exit(6), true},
		{"silent grep miss", "", exit(1), false},
		{"curl dns", "curl: (6) Could not resolve host: example.com", exit(6), true},
		{"sandbox-exec nested", "sandbox-exec: sandbox_apply: Operation not permitted", exit(71), true},
		{"helper failure", "zemi sandbox: landlock: landlock unavailable", exit(125), true},
		{"start error", "", errors.New("fork/exec /bin/x: operation not permitted"), true},
		{"landlock write", "sh: can't create /home/u/x: Permission denied", exit(1), linux},
		{"read-only mount", "touch: .git/config: Read-only file system", exit(1), linux},
		{"no route", "dial tcp 1.1.1.1:443: connect: network is unreachable", exit(1), linux},
		{"musl no route", "wget: can't connect to remote host (1.1.1.1): Network unreachable", exit(1), linux},
		{"musl rename a mount", "mv: can't rename '/tmp/t/proj/.git': Resource busy", exit(1), linux},
		{"not found", "sh: foo: Permission denied", exit(127), false},
		{"denial before the tail", "open x: operation not permitted\n" + strings.Repeat("ok\n", 3<<10), exit(1), false},
		{"go test assertion", "--- FAIL: TestOpen (0.00s)\n    open_test.go:12: want permission denied, got <nil>\nFAIL\nFAIL\tgithub.com/x/sandbox\t0.01s\nFAIL", exit(1), false},
		{"go test in a sandbox package", "ok  \tgithub.com/metafates/zemi/internal/sandbox\t0.4s\n--- FAIL: TestParse (0.00s)\n    parse_test.go:9: got 1\nFAIL", exit(1), false},
		{"pytest assertion", "E       AssertionError: assert 'Operation not permitted' in ''\n=========================== short test summary info ============================\nFAILED tests/test_sandbox.py::test_denied - AssertionError\n1 failed in 0.02s", exit(1), false},
		{"make test failure", "go test ./...\n--- FAIL: TestX (0.00s)\nFAIL\nmake: *** [Makefile:4: test] Error 1", exit(2), false},
		{"esbuild unresolved import", "X [ERROR] Could not resolve \"./utils\"\n\n    src/index.ts:1:22:", exit(1), false},
		{"vitest in a sandbox file", " FAIL  src/sandbox.test.ts > renders\nAssertionError: expected 'seccomp' to be 'landlock'", exit(1), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := LikelyDenied(c.out, c.err); got != c.want {
				t.Fatalf("LikelyDenied = %v", got)
			}
		})
	}
}

// A window program keeps its own terminal; the user's other terminals stay
// out of reach.
func TestTerminal(t *testing.T) {
	probeOrSkip(t)
	other, otherTTY, err := pty.Open()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer other.Close()
	defer otherTTY.Close()
	script := `stty -g >/dev/null && stty size && echo mine >/dev/tty && read l && echo got:$l
if "$0" __act write "$1" 2>/dev/null || "$0" __act open "$1" 2>/dev/null; then echo escaped; else echo confined; fi`
	cmd := exec.Command("/bin/sh", "-c", script, os.Args[0], otherTTY.Name())
	if err := Wrap(cmd, Policy{}); err != nil {
		t.Fatal(err)
	}
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); ptmx.Close() }()
	if _, err := ptmx.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1024)
		for {
			n, err := ptmx.Read(buf)
			out.Write(buf[:n])
			if err != nil || strings.Contains(out.String(), "escaped") || strings.Contains(out.String(), "confined") {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("no answer from the program")
	}
	for _, want := range []string{"24 80", "mine", "got:hi", "confined"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("no %q in:\n%s", want, out.String())
		}
	}
}

// A root inside another root's protected .zemi, and a session started in it.
func TestWorktreeRoot(t *testing.T) {
	b := probeOrSkip(t)
	if !b.EnforcesProtect {
		t.Skipf("%s does not enforce this", b.Name)
	}
	root := t.TempDir()
	zemi := filepath.Join(root, ".zemi")
	trees := filepath.Join(zemi, "worktrees")
	wt := filepath.Join(trees, "x")
	if err := os.MkdirAll(filepath.Join(wt, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outer := Policy{Write: []string{root, trees}, Protect: []string{zemi, filepath.Join(wt, ".git")}}
	inner := Policy{Write: []string{wt}, Protect: []string{filepath.Join(wt, ".git")}}
	cases := []struct {
		name, verb, arg string
		p               Policy
		allowed, skip   bool
	}{
		{"outer writes the worktree", "write", filepath.Join(wt, "pkg", "a.txt"), outer, true, false},
		{"outer writes .zemi", "write", filepath.Join(zemi, "init.lua"), outer, false, false},
		{"outer writes the worktree's .git", "write", filepath.Join(wt, ".git"), outer, false, false},
		{"outer creates the worktree's .zemi", "mkwrite", filepath.Join(wt, ".zemi", "init.lua"), outer, false, runtime.GOOS == "linux"},
		{"inner writes the worktree", "write", filepath.Join(wt, "pkg", "b.txt"), inner, true, false},
		{"inner creates a nested .git", "mkwrite", filepath.Join(wt, "pkg", ".git", "config"), inner, false, runtime.GOOS == "linux"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.skip {
				t.Skipf("%s does not enforce this", b.Name)
			}
			out, err := run(t, c.p, c.verb, c.arg)
			if c.allowed && err != nil {
				t.Fatalf("denied: %v\n%s", err, out)
			}
			if !c.allowed && (err == nil || !LikelyDenied(out, err)) {
				t.Fatalf("not denied: %v\n%s", err, out)
			}
		})
	}
}
