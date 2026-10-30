package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/metafates/zemi/internal/config"
	"github.com/metafates/zemi/internal/hostapi"
	"github.com/metafates/zemi/internal/ipc"
	"github.com/metafates/zemi/internal/perm"
	"github.com/metafates/zemi/internal/store"
)

// shortSockDir is a bind dir short enough for a sun_path on every platform;
// a test's own temp dir can outgrow the address limit on darwin.
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zemi-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// runSend runs zemi send against a crafted peers dir and returns its output
// and error, the way a script sees them.
func runSend(t *testing.T, dirs config.Dirs, args ...string) (string, error) {
	t.Helper()
	o := &options{dirs: dirs}
	cmd := sendCommand(o)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.RunE(cmd, args)
	return out.String(), err
}

// TestSendReadsPresence: the send loop tries fresh heartbeats first, probes
// an aged one only when nothing fresh answered (mtime has writers other than
// the window), and refuses honestly when no window answers.
func TestSendReadsPresence(t *testing.T) {
	dirs := config.Dirs{StateHome: t.TempDir(), ProjectDir: t.TempDir()}
	peers := filepath.Join(dirs.StateHome, "peers",
		filepath.Base(filepath.Dir(store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot()))))
	if err := os.MkdirAll(peers, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHb := func(name, sock string, fresh bool) {
		t.Helper()
		p := filepath.Join(peers, name)
		body := `{"working":false}`
		if sock != "" {
			body = `{"working":false,"sock":"` + sock + `"}`
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if !fresh {
			past := time.Now().Add(-hostapi.PeersTTL - time.Second)
			if err := os.Chtimes(p, past, past); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("no window is an honest refusal", func(t *testing.T) {
		_, err := runSend(t, dirs, "hello")
		if err == nil || err.Error() != "no zemi window is live in "+perm.TildePath(dirs.TrustRoot()) {
			t.Fatalf("refusal = %v, want the no-window error", err)
		}
	})

	t.Run("a fresh live window is delivered", func(t *testing.T) {
		path := filepath.Join(shortSockDir(t), "w.sock")
		ln, err := ipc.Listen(path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if req, err := ipc.ReadRequest(conn); err == nil && req.Text == "hello" {
				_ = ipc.WriteReply(conn, ipc.Reply{OK: true})
			}
		}()
		writeHb("live.json", path, true)
		out, err := runSend(t, dirs, "hello")
		if err != nil || out != "delivered: a turn starts in the window\n" {
			t.Fatalf("deliver = %q, %v", out, err)
		}
	})

	t.Run("an aged heartbeat is probed before the refusal", func(t *testing.T) {
		path := filepath.Join(shortSockDir(t), "w.sock")
		ln, err := ipc.Listen(path)
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if req, err := ipc.ReadRequest(conn); err == nil && req.Text == "aged" {
				_ = ipc.WriteReply(conn, ipc.Reply{OK: true, Queued: true})
			}
		}()
		writeHb("aged.json", path, false)
		out, err := runSend(t, dirs, "aged")
		if err != nil || out != "delivered: the message joined the window's running turn\n" {
			t.Fatalf("deliver = %q, %v", out, err)
		}
	})

	t.Run("a fresh dead window refuses", func(t *testing.T) {
		writeHb("dead.json", filepath.Join(shortSockDir(t), "gone.sock"), true)
		_, err := runSend(t, dirs, "nobody")
		if err == nil || err.Error() != "no zemi window is live in "+perm.TildePath(dirs.TrustRoot()) {
			t.Fatalf("refusal = %v, want the no-window error", err)
		}
	})
}
