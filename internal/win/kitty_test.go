package win

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
	"github.com/charmbracelet/x/vt"
)

func apc(s string) string { return "\x1b_G" + s + "\x1b\\" }

// cells is n placeholder cells of row r, program-written, starting at column 0.
func cells(r, n int) string {
	var b strings.Builder
	for c := range n {
		b.WriteRune(kitty.Placeholder)
		b.WriteRune(kitty.Diacritic(r))
		b.WriteRune(kitty.Diacritic(c))
	}
	return b.String()
}

func TestKittyGraphics(t *testing.T) {
	cases := []struct {
		name   string
		off    bool   // the host draws no placeholders
		out    string // what the program prints
		gfx    []string
		reply  string
		placed bool   // a 3x2 image at the top left, END on the row below
		text   string // Text() when not placed
	}{
		{
			name:   "direct transmit and place moves the cursor below",
			out:    apc("a=T,f=100,c=3,r=2,q=2;iVBO") + "\r\nEND",
			gfx:    []string{apc("a=t,i=16,q=2,f=100,m=0;iVBO"), apc("a=p,U=1,i=16,p=1,c=3,r=2,q=2")},
			placed: true,
		},
		{
			name:   "the program's saved cursor survives placement",
			out:    "\x1b[3;1H\x1b7\x1b[H" + apc("a=T,f=100,c=3,r=2,q=2;iVBO") + "\x1b8END",
			placed: true,
		},
		{
			name: "chunks join into one transmit",
			out:  apc("a=T,f=100,c=3,r=2,m=1;AAAA") + apc("m=1;BBBB") + apc("m=0;CA==") + "\r\nEND",
			gfx:  []string{";AAAABBBBCA==\x1b\\"}, placed: true,
		},
		{
			name: "program placeholders map to the host id",
			out:  apc("a=T,U=1,i=5,f=100,c=3,r=2,q=2;iVBO") + "\x1b[38;5;5m" + cells(0, 3) + "\r\n" + cells(1, 3) + "\x1b[0m\r\nEND",
			gfx:  []string{apc("a=p,U=1,i=16,p=1,c=3,r=2,q=2")}, placed: true,
		},
		{
			name: "a 32-bit id keys placeholders by its low 24 bits",
			out:  apc("a=T,U=1,i=262955020,f=100,c=3,r=2;iVBO") + "\x1b[38;2;172;96;12m" + cells(0, 3) + "\r\n" + cells(1, 3) + "\x1b[0m\r\nEND",
			gfx:  []string{apc("a=p,U=1,i=16,p=1,c=3,r=2,q=2")}, reply: apc("i=262955020;OK"), placed: true,
		},
		{
			name: "file transmission is refused",
			out:  apc("a=T,t=f,i=4,c=1,r=1;L2V0Yy9wYXNzd2Q=") + "END",
			text: "END", reply: apc("i=4;EINVAL:only direct transmission"),
		},
		{name: "non-base64 payload is refused", out: apc("a=T,c=1,r=1;!!!!") + "END", text: "END"},
		{
			name: "placeholders of an image the window never sent are blank",
			out:  "\x1b[38;5;16m" + cells(0, 3) + "\x1b[0mEND",
			text: "   END",
		},
		{
			name: "delete frees the image and blanks its cells",
			out:  apc("a=T,i=7,f=100,c=3,r=2,q=2;iVBO") + apc("a=d,d=I,i=7,q=2") + "\r\nEND",
			gfx:  []string{apc("a=d,d=I,i=16,q=2")}, text: "\n\nEND",
		},
		{name: "query", out: apc("a=q,i=31;AAAA") + "END", reply: apc("i=31;OK"), text: "END"},
		{name: "query for shared memory is refused", out: apc("a=q,t=s,i=31;AAAA") + "END", reply: apc("i=31;EINVAL:only direct transmission"), text: "END"},
		{
			name: "no host support",
			off:  true, out: apc("a=T,i=31,c=3,r=2;iVBO") + "END",
			reply: apc("i=31;ENOTSUPPORTED:no image support in this terminal"), text: "END",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{}
			m.SetKitty(!tc.off)
			w := &Window{m: m, ID: 1, term: vt.NewEmulator(20, 6), w: 20, h: 6}
			w.term.RegisterApcHandler(w.graphics)
			var mu sync.Mutex
			var reply strings.Builder
			go func() {
				buf := make([]byte, 256)
				for {
					n, err := w.term.Read(buf)
					mu.Lock()
					reply.Write(buf[:n])
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
			defer w.term.InputPipe().(io.Closer).Close()
			// split after every ESC, so an ST lands across two reads
			for _, p := range strings.SplitAfter(tc.out, "\x1b") {
				w.write([]byte(p))
			}
			gfx := m.TakeGraphics()
			if len(tc.gfx) == 0 && !tc.placed && gfx != "" {
				t.Fatalf("host got %q", gfx)
			}
			for _, want := range tc.gfx {
				if !strings.Contains(gfx, want) {
					t.Fatalf("host got %q, want %q in it", gfx, want)
				}
			}
			scr := w.Screen()
			if tc.placed {
				for y := range 2 {
					sp := scr.Rows[y][0]
					if sp.SGR != "38;5;16" || sp.Text != cells(y, 3) {
						t.Fatalf("row %d: %+v", y, scr.Rows[y])
					}
				}
				tc.text = "\n\nEND"
				m.dropImages(w)
				if g := m.TakeGraphics(); !strings.Contains(g, apc("a=d,d=I,i=16,q=2")) {
					t.Fatalf("close queued %q", g)
				}
			}
			if got := w.Text(); got != tc.text {
				t.Fatalf("text %q, want %q", got, tc.text)
			}
			deadline := time.Now().Add(time.Second)
			for {
				mu.Lock()
				got := reply.String()
				mu.Unlock()
				if got == tc.reply || time.Now().After(deadline) {
					if got != tc.reply {
						t.Fatalf("reply %q, want %q", got, tc.reply)
					}
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

// TestImageIDsEvictOldest: past the 240 host ids the window's oldest image
// is deleted before its id is reused, resent or not, and a closed window
// queues nothing.
func TestImageIDsEvictOldest(t *testing.T) {
	m := &Manager{}
	w := &Window{m: m, ID: 1}
	for i := range 241 {
		m.transmit(w, uint32(i+1), map[string]string{}, []byte("x"))
	}
	if g := m.TakeGraphics(); !strings.Contains(g, apc("a=d,d=I,i=16,q=2")) || m.hostID(1, 241) != 16 || m.hostID(1, 1) != 0 {
		t.Fatal("the oldest id was not evicted and reused")
	}
	// resending an image keeps its id and leaves the oldest next in line
	m.transmit(w, 100, map[string]string{}, []byte("x"))
	m.transmit(w, 999, map[string]string{}, []byte("x"))
	if m.hostID(1, 100) == 0 || m.hostID(1, 2) != 0 || m.hostID(1, 101) == 0 {
		t.Fatal("a resent image moved eviction off the oldest")
	}
	// another window's churn evicts its own images, not this one's
	other := &Window{m: m, ID: 2}
	for range 300 {
		m.transmit(other, 0, map[string]string{}, []byte("x"))
	}
	if m.hostID(1, 100) == 0 {
		t.Fatal("one window's images evicted another's")
	}
	m.dropImages(w)
	m.TakeGraphics()
	if h, _ := m.transmit(w, 9, map[string]string{}, []byte("x")); h != 0 || m.TakeGraphics() != "" {
		t.Fatal("a closed window queued an image")
	}
}

// TestZeroWidthCellKeepsColumns: a combining mark vt leaves in its own cell
// still takes a column, so the rest of the row does not shift left.
func TestZeroWidthCellKeepsColumns(t *testing.T) {
	w := &Window{term: vt.NewEmulator(10, 2), w: 10, h: 2}
	w.term.Write([]byte("abe\u0301\x1b[2;1H"))
	n := 0
	for _, sp := range w.Screen().Rows[0] {
		n += ansi.StringWidth(sp.Text)
	}
	if n != 10 {
		t.Fatalf("row 0 is %d cells: %+v", n, w.Screen().Rows[0])
	}
}
