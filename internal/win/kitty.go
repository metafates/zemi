package win

// kitty.go passes kitty graphics through to the host terminal. A program's
// image goes to the host under an id zemi owns, as a virtual placement, and
// the emulator gets Unicode placeholder cells where the image goes. The host
// draws the image wherever those cells land, so moving, hiding, clipping and
// scrolling a window need nothing image-specific; only close and eviction
// delete. Everything sent to the host has q=2, so its replies never reach
// zemi's input.

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"fmt"
	"image/color"
	"image/png"
	"regexp"
	"strconv"
	"strings"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/kitty"
)

const (
	// host ids 16..255, so a placeholder's fg is 38;5;N and survives a
	// 256-color profile
	firstImageID = 16
	maxCells     = 297 // the diacritic table: rows and columns of one image
	maxUpload    = 32 << 20
	maxQueued    = 64 << 20 // sequences waiting for the UI to write them
	// NOTE: a guess at the host's cell size, for images sent without c and r
	// and as the pty's pixel size; the host's CSI 16 t reply is the upgrade.
	cellW, cellH = 10, 20
)

// imgSlot is the owner of one host image id.
type imgSlot struct {
	win    int    // 0 = free
	key    uint32 // the program's i=, or 1<<23|host id when it gave none
	pw, ph int    // pixel size, for a placement without c and r
	seq    int    // when the id was last taken, kept while free: eviction order
}

// upload is a chunked transmission in progress.
type upload struct {
	keys map[string]string
	data []byte
	bad  bool // refused: the rest of its chunks are dropped
}

var placeholderRun = regexp.MustCompile("\U0010EEEE\\p{Mn}*")

// StripPlaceholders turns image placeholder cells into blanks, for text read
// off a window.
func StripPlaceholders(s string) string {
	if !strings.ContainsRune(s, kitty.Placeholder) {
		return s
	}
	return placeholderRun.ReplaceAllString(s, " ")
}

// SetKitty says whether the host terminal draws kitty placeholders; windows
// opened after it see TERM_PROGRAM=ghostty, which image tools read as that.
func (m *Manager) SetKitty(on bool) { m.kitty.Store(on) }

// Kitty reports what SetKitty set.
func (m *Manager) Kitty() bool { return m.kitty.Load() }

// TakeGraphics returns the sequences queued for the host terminal and
// clears the queue.
func (m *Manager) TakeGraphics() string {
	m.gmu.Lock()
	defer m.gmu.Unlock()
	s := m.gfx.String()
	m.gfx.Reset()
	return s
}

// graphics is the emulator's APC handler; it runs inside term.Write, under
// w.mu.
func (w *Window) graphics(data []byte) bool {
	if len(data) == 0 || data[0] != 'G' {
		return false
	}
	head, payload, _ := bytes.Cut(data[1:], []byte{';'})
	keys := map[string]string{}
	for kv := range strings.SplitSeq(string(head), ",") {
		k, v, _ := strings.Cut(kv, "=")
		keys[k] = v
	}
	raw, err := base64.StdEncoding.DecodeString(string(payload))
	up := w.up
	if up == nil {
		up = &upload{keys: keys}
	}
	// the parser keeps 4 MB of a sequence, so a longer one arrives cut
	if err != nil || len(data) >= 4<<20 || len(up.data)+len(raw) > maxUpload {
		up.bad = true
	}
	if !up.bad {
		up.data = append(up.data, raw...)
	}
	if keys["m"] == "1" {
		w.up = up
		return true
	}
	w.up = nil
	if !up.bad {
		w.kittyCommand(up.keys, up.data)
	}
	return true
}

// kittyCommand acts on one whole command. Only direct base64 transmission
// and the keys it names reach the host: a file path (t=f, t=t, t=s) would
// have the unsandboxed host terminal read it on the program's behalf.
func (w *Window) kittyCommand(k map[string]string, data []byte) {
	var id uint32
	if s := k["i"]; s != "" {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil || n == 0 {
			return
		}
		id = uint32(n)
	}
	// NOTE: a placeholder's fg carries the low 24 bits, so the slot is keyed
	// by them and two ids that share them collide within one window
	key := id & 0xFFFFFF
	// kitty answers only commands with an id; q=1 hides OK, q=2 everything
	reply := func(msg string) {
		if id == 0 || k["q"] == "2" || k["q"] == "1" && msg == "OK" {
			return
		}
		_, _ = fmt.Fprintf(w.term.InputPipe(), "\x1b_Gi=%d;%s\x1b\\", id, msg)
	}
	if !w.m.kitty.Load() {
		reply("ENOTSUPPORTED:no image support in this terminal")
		return
	}
	var h int
	switch a := k["a"]; a {
	case "q", "", "t", "T":
		// a query checks the medium too, so a tool that probes for one picks direct
		if t := k["t"]; t != "" && t != "d" {
			reply("EINVAL:only direct transmission")
			return
		}
		if a == "q" {
			reply("OK")
			return
		}
		if h, key = w.m.transmit(w, key, k, data); h == 0 {
			reply("ENOSPC:too many images")
			return
		}
		reply("OK")
		if a != "T" {
			return
		}
	case "p":
		if h = w.m.hostID(w.ID, key); h == 0 {
			reply("ENOENT:no such image")
			return
		}
	case "d":
		// NOTE: deletes by cursor, cell or z-index are ignored; clearing the
		// cells removes the image anyway
		switch d := k["d"]; d {
		case "", "a", "A", "i", "I":
			w.m.deleteImages(w, key, d == "A" || d == "I")
		}
		return
	default:
		return
	}
	cols, rows := atoi(k["c"]), atoi(k["r"])
	if cols <= 0 || rows <= 0 {
		pw, ph := w.m.pixels(h)
		cols, rows = (pw+cellW-1)/cellW, (ph+cellH-1)/cellH
	}
	cols, rows = min(cols, maxCells), min(rows, maxCells)
	if cols <= 0 || rows <= 0 {
		return
	}
	// one fixed placement id: a re-place replaces the last one, where a host
	// would add another and keep drawing the first size
	w.m.queue(fmt.Sprintf("\x1b_Ga=p,U=1,i=%d,p=1,c=%d,r=%d,q=2\x1b\\", h, cols, rows))
	if k["U"] != "1" { // with U=1 the program writes its own placeholders
		w.place = w.placeholders(key, cols, rows, k["C"] == "1")
	}
}

// placeholders draws an image of cols x rows cells at the cursor, moving
// the cursor after it the way kitty does unless noMove. It sets the cells
// directly, so the program's pen and saved cursor stay as it left them.
func (w *Window) placeholders(key uint32, cols, rows int, noMove bool) func() {
	return func() {
		x0 := w.term.CursorPosition().X
		rows := min(rows, w.h)
		drawn := min(cols, w.w-x0)
		if rows > 1 { // make room first, scrolling like the lines kitty would index
			fmt.Fprintf(w.term, "%s\x1b[%dA", strings.Repeat("\n", rows-1), rows-1)
		}
		y0 := w.term.CursorPosition().Y
		st := uv.Style{Fg: color.RGBA{uint8(key >> 16), uint8(key >> 8), uint8(key), 0xff}}
		for r := range rows {
			for c := range drawn {
				w.term.SetCell(x0+c, y0+r, &uv.Cell{Content: string([]rune{kitty.Placeholder, kitty.Diacritic(r), kitty.Diacritic(c)}), Width: 1, Style: st})
			}
		}
		if noMove {
			return
		}
		if rows > 1 {
			fmt.Fprintf(w.term, "\x1b[%dB", rows-1)
		}
		if x0+cols >= w.w {
			fmt.Fprint(w.term, "\r\n")
		} else {
			fmt.Fprintf(w.term, "\x1b[%dG", x0+cols+1)
		}
	}
}

// placeholderCell maps a placeholder cell to its host image: the fg the
// program wrote names its own key, so a window shows only images it sent.
// A placeholder with no image, or joined after another rune, becomes a blank.
func (w *Window) placeholderCell(text string, st uv.Style) (string, uv.Style) {
	if !strings.HasPrefix(text, string(kitty.Placeholder)) {
		return " ", uv.Style{}
	}
	var key uint32
	switch c := st.Fg.(type) {
	case nil:
	case ansi.BasicColor:
		key = uint32(c)
	case ansi.IndexedColor:
		key = uint32(c)
	case color.Color:
		r, g, b, _ := c.RGBA()
		key = r>>8<<16 | g>>8<<8 | b>>8
	}
	h := w.m.hostID(w.ID, key)
	if h == 0 {
		return " ", uv.Style{}
	}
	// the third diacritic would name the high byte of another id
	if r := []rune(text); len(r) > 3 {
		text = string(r[:3])
	}
	return text, uv.Style{Fg: ansi.IndexedColor(h)}
}

// transmit gives the window's image a host id and queues it; 0 when the
// window is closed or the queue is full. A key the window already used keeps
// its id and its age, and the host replaces the image; otherwise the id free
// longest is taken, and when none is, the window's own oldest image is
// evicted, another window's only when it holds none, so a program that
// churns images cannot blank another window's.
func (m *Manager) transmit(w *Window, key uint32, k map[string]string, data []byte) (int, uint32) {
	m.gmu.Lock()
	defer m.gmu.Unlock()
	if w.gone || m.gfx.Len() > maxQueued {
		return 0, 0
	}
	h := 0
	for i := firstImageID; i < len(m.slots) && key != 0; i++ {
		if m.slots[i].win == w.ID && m.slots[i].key == key {
			h = i
		}
	}
	seq := m.slots[h].seq
	if h == 0 {
		var free, own, other int
		for i := firstImageID; i < len(m.slots); i++ {
			s, pick := m.slots[i], &other
			if s.win == 0 {
				pick = &free
			} else if s.win == w.ID {
				pick = &own
			}
			if *pick == 0 || s.seq < m.slots[*pick].seq {
				*pick = i
			}
		}
		if h = cmp.Or(free, own, other); m.slots[h].win != 0 {
			m.deleteSlot(h)
		}
		m.next++
		seq = m.next
	}
	if key == 0 {
		key = 1<<23 | uint32(h)
	}
	opts := []string{"a=t", fmt.Sprintf("i=%d", h), "q=2"}
	for _, name := range []string{"f", "s", "v"} {
		if n := atoi(k[name]); n > 0 {
			opts = append(opts, fmt.Sprintf("%s=%d", name, n))
		}
	}
	if k["o"] == "z" {
		opts = append(opts, "o=z")
	}
	slot := imgSlot{win: w.ID, key: key, seq: seq, pw: atoi(k["s"]), ph: atoi(k["v"])}
	if k["f"] == "100" && k["o"] == "" {
		if cfg, err := png.DecodeConfig(bytes.NewReader(data)); err == nil {
			slot.pw, slot.ph = cfg.Width, cfg.Height
		}
	}
	m.slots[h] = slot
	enc := base64.StdEncoding.EncodeToString(data)
	for {
		n := min(4096, len(enc))
		more := "m=0"
		if n < len(enc) {
			more = "m=1"
		}
		m.gfx.WriteString(ansi.KittyGraphics([]byte(enc[:n]), append(opts, more)...))
		enc, opts = enc[n:], []string{"q=2"}
		if enc == "" {
			return h, key
		}
	}
}

// deleteImages removes the window's image with key, or all of them when key
// is 0: placements only, or the data too when free.
func (m *Manager) deleteImages(w *Window, key uint32, free bool) {
	m.gmu.Lock()
	defer m.gmu.Unlock()
	for h := firstImageID; h < len(m.slots); h++ {
		if s := m.slots[h]; s.win != w.ID || key != 0 && s.key != key {
			continue
		}
		if free {
			m.deleteSlot(h)
		} else if m.gfx.Len() <= maxQueued {
			fmt.Fprintf(&m.gfx, "\x1b_Ga=d,d=i,i=%d,q=2\x1b\\", h)
		}
	}
}

// dropImages frees a closed window's images; the window can queue no more.
func (m *Manager) dropImages(w *Window) {
	m.gmu.Lock()
	w.gone = true
	m.gmu.Unlock()
	m.deleteImages(w, 0, true)
}

// deleteSlot queues the delete of a host image and frees its id; gmu held.
func (m *Manager) deleteSlot(h int) {
	fmt.Fprintf(&m.gfx, "\x1b_Ga=d,d=I,i=%d,q=2\x1b\\", h)
	m.slots[h] = imgSlot{seq: m.slots[h].seq}
}

func (m *Manager) hostID(win int, key uint32) int {
	m.gmu.Lock()
	defer m.gmu.Unlock()
	for h := firstImageID; h < len(m.slots); h++ {
		if s := m.slots[h]; s.win == win && s.key == key {
			return h
		}
	}
	return 0
}

func (m *Manager) pixels(h int) (int, int) {
	m.gmu.Lock()
	defer m.gmu.Unlock()
	return m.slots[h].pw, m.slots[h].ph
}

func (m *Manager) queue(s string) {
	m.gmu.Lock()
	defer m.gmu.Unlock()
	if m.gfx.Len() <= maxQueued {
		m.gfx.WriteString(s)
	}
}

// atoi reads a small non-negative number; anything else is 0.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 1<<20 {
		return 0
	}
	return n
}
