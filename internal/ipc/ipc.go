// Package ipc is the stream half of talking between zemi processes: one
// socket per window beside its presence heartbeat, which a second zemi dials
// to deliver a prompt. AF_UNIX covers every platform zemi builds for —
// Windows 10 1803+ included, through net's own windows implementation — so
// there is one wire, not one per OS.
package ipc

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrLive names a socket another live window owns; ErrDead names a path
// nothing listens on, which a caller treats as absence.
var (
	ErrLive = errors.New("another zemi window owns this socket")
	ErrDead = errors.New("no zemi window is listening")
)

// MaxFrame caps one frame's bytes: the window's own message ceiling
// (hostapi's maxMessageChars) is smaller, so this bounds only the read.
const MaxFrame = 1 << 20

// Listen binds path, reclaiming a socket file a crashed window left behind:
// the file outlives the process, so a live one is probed first and only an
// unanswerable name is removed and bound again.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		if conn, err := net.Dial("unix", path); err == nil {
			_ = conn.Close()
			return nil, ErrLive
		}
		_ = os.Remove(path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Dial connects to the socket at path; a missing or unanswerable listener is
// ErrDead, and the timeout bounds the dial alone.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err == nil {
		return conn, nil
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, ErrDead
	}
	return nil, err
}

// Request is the one frame a client sends: the text to deliver, a presence
// poke, the write handed over, a viewer's ask for it, or the writing window's
// live stream pushed to the ones watching.
type Request struct {
	Text   string         `json:"text"`
	Hello  bool           `json:"hello,omitempty"`  // presence poke: re-read the siblings now
	Yours  bool           `json:"yours,omitempty"`  // the write was handed to this window: re-read the lease now
	Yield  bool           `json:"yield,omitempty"`  // ask for the write; Origin and Sock name the asker
	Origin string         `json:"origin,omitempty"` // the asking window's per-boot id
	Sock   string         `json:"sock,omitempty"`   // the asking window's socket
	Stream jsontext.Value `json:"stream,omitempty"` // the writer's live turn, whole: a watching window draws it as its own
}

// Reply is the one frame the window answers with.
type Reply struct {
	OK     bool   `json:"ok"`
	Queued bool   `json:"queued,omitempty"` // the message joined a running turn
	Parked bool   `json:"parked,omitempty"` // the message parked on a viewer: it sends once the write comes to it
	Ran    bool   `json:"ran,omitempty"`    // a command ran instead
	Error  string `json:"error,omitempty"`
	Note   string `json:"note,omitempty"` // what the window is on, for windows a sender cannot tell apart
}

// RoundTrip writes one request and reads one reply, both deadline-bounded by
// timeout from now. One connection carries one frame each way.
func RoundTrip(conn net.Conn, req *Request, timeout time.Duration) (*Reply, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	line, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	var reply Reply
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

// ReadRequest reads the one frame a connection carries.
func ReadRequest(conn net.Conn) (*Request, error) {
	line, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// WriteReply writes the one frame the window answers with.
func WriteReply(conn net.Conn, reply Reply) error {
	raw, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(raw, '\n'))
	return err
}

// readFrame reads one newline-delimited frame, refusing one past MaxFrame.
func readFrame(conn net.Conn) (string, error) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), MaxFrame)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", err
		}
		return "", io.ErrUnexpectedEOF
	}
	return sc.Text(), nil
}
