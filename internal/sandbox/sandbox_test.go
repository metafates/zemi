//go:build darwin || linux

package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as the sandboxed program (__act) and, on Linux,
// as the helper zemi's main dispatches to.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__sandbox":
			os.Exit(Helper(os.Args[2:]))
		case "__act":
			if err := act(os.Args[2], os.Args[3]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func act(verb, arg string) error {
	switch verb {
	case "write":
		return os.WriteFile(arg, []byte("x"), 0o644)
	case "read":
		_, err := os.ReadFile(arg)
		return err
	case "rename":
		return os.Rename(arg, arg+".moved")
	case "dial":
		c, err := net.DialTimeout("tcp", arg, 3*time.Second)
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
	seatbelt := b.Name == "seatbelt"
	root, outside, hide, state := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	scratch := filepath.Join(state, "scratch")
	for _, d := range []string{filepath.Join(root, ".git"), scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(root, "plain.txt"), filepath.Join(root, ".env"), filepath.Join(hide, "id")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := Policy{
		Write:    []string{root, scratch},
		Protect:  []string{filepath.Join(root, ".git"), state},
		Hide:     []string{hide},
		Loopback: true,
	}
	_, caseFolds := os.Stat(filepath.Join(root, ".GIT"))
	cases := []struct {
		name, verb, arg string
		allowed, skip   bool
	}{
		{"write inside a root", "write", filepath.Join(root, "a.txt"), true, false},
		{"read a plain file", "read", filepath.Join(root, "plain.txt"), true, false},
		{"write in a root under a protected dir", "write", filepath.Join(scratch, "a.txt"), true, false},
		{"write outside the roots", "write", filepath.Join(outside, "a.txt"), false, false},
		{"write a protected dir", "write", filepath.Join(state, "a.txt"), false, false},
		{"write .git/config", "write", filepath.Join(root, ".git", "config"), false, !seatbelt},
		{"write .GIT/config", "write", filepath.Join(root, ".GIT", "config"), false, !seatbelt || caseFolds != nil},
		{"rename .git", "rename", filepath.Join(root, ".git"), false, !seatbelt},
		{"read a hidden dir", "read", filepath.Join(hide, "id"), false, false},
		{"read .env", "read", filepath.Join(root, ".env"), false, !b.HidesSecretFiles},
		{"listen and dial loopback", "loopback", "", true, !seatbelt},
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
		// EPERM, not a timeout: the connect never left the machine
		if err == nil || !strings.Contains(strings.ToLower(out), "operation not permitted") {
			t.Fatalf("dial %s: %v\n%s", addr, err, out)
		}
	}
	dial("192.0.2.1:9", true)
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

func nonLoopbackIP() net.IP {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP
		}
	}
	return nil
}

func TestPidKept(t *testing.T) {
	probeOrSkip(t)
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
	if strings.TrimSpace(line) != fmt.Sprint(pid) {
		t.Fatalf("program pid %s, started %d", line, pid)
	}
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Fatalf("pgid %d, %v; want %d", pgid, err, pid)
	}
}

func TestLikelyDenied(t *testing.T) {
	exit := func(code int) error {
		return exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	}
	for _, c := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"success", "operation not permitted", nil, false},
		{"write denied", "open x: operation not permitted", exit(1), true},
		{"read-only fs", "Read-only file system", exit(1), true},
		{"dns", "curl: (6) Could not resolve host: example.com", exit(6), true},
		{"helper failure", "zemi sandbox: landlock: landlock unavailable", exit(125), true},
		{"not found", "sh: foo: Permission denied", exit(127), false},
		{"usage", "permission denied", exit(2), false},
		{"plain failure", "FAIL: TestX", exit(1), false},
		{"start error", "", errors.New("fork/exec /bin/x: operation not permitted"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := LikelyDenied(c.out, c.err); got != c.want {
				t.Fatalf("LikelyDenied = %v", got)
			}
		})
	}
}
