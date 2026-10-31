package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/metafates/zemi/internal/config"
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

// runSend runs zemi send against this project's store and returns its output
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

// TestSendDialsTheLease: the lease row names the one window a delivery may
// reach; a row that moved between the read and the dial is re-read once, so
// a handover in flight still delivers.
func TestSendDialsTheLease(t *testing.T) {
	dirs := config.Dirs{StateHome: t.TempDir(), ProjectDir: t.TempDir()}
	dbPath := store.ProjectDBPath(dirs.StateHome, dirs.TrustRoot())
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	var db *store.DB
	// claim plants the lease on a socket, the way a live window's boot does
	claim := func(origin, sock string) {
		t.Helper()
		if db == nil {
			d, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			db = d
			t.Cleanup(func() { _ = d.Close() })
		}
		l, err := db.ReadLeader()
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if _, err := db.ClaimLeader(origin, sock, l); err != nil {
			t.Fatal(err)
		}
	}
	// listen answers one request on a fresh socket and returns its path
	listen := func(reply func() ipc.Reply) string {
		t.Helper()
		path := filepath.Join(shortSockDir(t), "w.sock")
		ln, err := ipc.Listen(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if _, err := ipc.ReadRequest(conn); err == nil {
				_ = ipc.WriteReply(conn, reply())
			}
		}()
		return path
	}

	t.Run("no writer is an honest refusal", func(t *testing.T) {
		_, err := runSend(t, dirs, "hello")
		if err == nil || err.Error() != "no zemi window is writing this chat in "+perm.TildePath(dirs.TrustRoot()) {
			t.Fatalf("refusal = %v, want the no-writer error", err)
		}
	})

	t.Run("the holder is delivered and names its live task", func(t *testing.T) {
		sock := listen(func() ipc.Reply { return ipc.Reply{OK: true, Note: "writing the readme"} })
		claim("a", sock)
		out, err := runSend(t, dirs, "hello")
		if err != nil || out != "delivered: a turn starts in the window\n  · writing the readme\n" {
			t.Fatalf("deliver = %q, %v", out, err)
		}
	})

	t.Run("a holder that will not answer names the handover", func(t *testing.T) {
		claim("b", filepath.Join(shortSockDir(t), "gone.sock"))
		_, err := runSend(t, dirs, "nobody")
		if err == nil || err.Error() != "the writable window did not answer; it may be handing over - try again" {
			t.Fatalf("refusal = %v, want the handover error", err)
		}
	})

	t.Run("a row that moved under the dial is re-read once", func(t *testing.T) {
		d := listen(func() ipc.Reply { return ipc.Reply{OK: true} })
		c := listen(func() ipc.Reply {
			// the holder hands the lease over, then refuses: the row the
			// delivery read is stale the moment it answers
			l, _ := db.ReadLeader()
			_, _ = db.ClaimLeader("d", d, l)
			return ipc.Reply{OK: false, Error: "another window writes this chat"}
		})
		claim("c", c)
		out, err := runSend(t, dirs, "hello")
		if err != nil || out != "delivered: a turn starts in the window\n" {
			t.Fatalf("deliver = %q, %v", out, err)
		}
	})
}
