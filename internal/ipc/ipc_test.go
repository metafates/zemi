package ipc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortDir is a bind dir short enough for a sun_path on every platform: a
// test's own temp dir can outgrow the address limit on darwin.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zemi-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestIPC(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"a fresh name binds", func(t *testing.T) {
			path := filepath.Join(shortDir(t), "w.sock")
			ln, err := Listen(path)
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			defer ln.Close()
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("no socket file at %s: %v", path, err)
			}
		}},
		{"a live socket refuses a second bind", func(t *testing.T) {
			path := filepath.Join(shortDir(t), "w.sock")
			ln, err := Listen(path)
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			defer ln.Close()
			if _, err := Listen(path); !errors.Is(err, ErrLive) {
				t.Fatalf("second bind: got %v, want ErrLive", err)
			}
		}},
		{"a socket a crashed window left is reclaimed", func(t *testing.T) {
			path := filepath.Join(shortDir(t), "w.sock")
			ln, err := Listen(path)
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			ln.Close() // the file survives the close, like a crashed process's
			ln2, err := Listen(path)
			if err != nil {
				t.Fatalf("rebind over the dead socket: %v", err)
			}
			ln2.Close()
		}},
		{"dial names a dead socket", func(t *testing.T) {
			if _, err := Dial(filepath.Join(shortDir(t), "gone.sock"), time.Second); !errors.Is(err, ErrDead) {
				t.Fatalf("dial: got %v, want ErrDead", err)
			}
		}},
		{"a round trip carries the text and the verdict", func(t *testing.T) {
			path := filepath.Join(shortDir(t), "w.sock")
			ln, err := Listen(path)
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			defer ln.Close()
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				req, err := ReadRequest(conn)
				if err != nil || req.Text != "check the tests" {
					return
				}
				_ = WriteReply(conn, Reply{OK: true, Queued: true})
			}()
			conn, err := Dial(path, time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			reply, err := RoundTrip(conn, &Request{Text: "check the tests"}, 2*time.Second)
			if err != nil {
				t.Fatalf("round trip: %v", err)
			}
			if !reply.OK || !reply.Queued {
				t.Fatalf("reply: got %+v, want ok and queued", reply)
			}
		}},
		{"an over-long frame is refused", func(t *testing.T) {
			path := filepath.Join(shortDir(t), "w.sock")
			ln, err := Listen(path)
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			defer ln.Close()
			refused := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					refused <- err
					return
				}
				defer conn.Close()
				if _, err := ReadRequest(conn); err == nil {
					refused <- errors.New("the over-long frame was accepted")
					return
				}
				refused <- nil
			}()
			conn, err := Dial(path, time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_, _ = conn.Write(make([]byte, MaxFrame+1)) // the refusal closes the conn, which unblocks the write
			if err := <-refused; err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t) })
	}
}
