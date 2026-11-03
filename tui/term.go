package tui

import (
	"bufio"
	"io"
	"os"

	"golang.org/x/term"
)

// key is one decoded keypress: a rune, or a named key for the sequences a
// terminal sends for arrows and such.
type key struct {
	r    rune
	name string // "up" | "down" | "left" | "right" | "enter" | "esc" | "pgup" | "pgdn" | ""
}

// readKeys decodes stdin into keys until it closes. Escape sequences are
// matched narrowly (CSI arrows and page keys); a lone ESC is "esc".
func readKeys(r io.Reader, out chan<- key) {
	br := bufio.NewReader(r)
	defer close(out)
	for {
		c, err := br.ReadByte()
		if err != nil {
			return
		}
		switch c {
		case '\r', '\n':
			out <- key{name: "enter"}
		case 0x1b:
			if br.Buffered() == 0 {
				out <- key{name: "esc"}
				continue
			}
			b, _ := br.ReadByte()
			if b != '[' {
				br.UnreadByte() // a lone escape followed by a key: both are keys
				out <- key{name: "esc"}
				continue
			}
			var seq []byte
			for {
				x, err := br.ReadByte()
				if err != nil {
					return
				}
				seq = append(seq, x)
				if x >= 0x40 && x <= 0x7e {
					break
				}
			}
			switch string(seq) {
			case "A":
				out <- key{name: "up"}
			case "B":
				out <- key{name: "down"}
			case "C":
				out <- key{name: "right"}
			case "D":
				out <- key{name: "left"}
			case "5~":
				out <- key{name: "pgup"}
			case "6~":
				out <- key{name: "pgdn"}
			}
		default:
			if c < 0x80 {
				out <- key{r: rune(c)}
				continue
			}
			br.UnreadByte()
			r, _, err := br.ReadRune()
			if err != nil {
				return
			}
			out <- key{r: r}
		}
	}
}

// terminal brackets the screen: raw mode on stdin, the alternate screen and
// a hidden cursor on stdout, both restored on every exit path. Off a real
// file (tests) it is a no-op with a fixed size.
type terminal struct {
	in, out  *os.File
	state    *term.State
	fallback [2]int
}

func openTerminal(in io.Reader, out io.Writer, fallback [2]int) *terminal {
	t := &terminal{fallback: fallback}
	t.in, _ = in.(*os.File)
	t.out, _ = out.(*os.File)
	if t.in != nil && term.IsTerminal(int(t.in.Fd())) {
		if st, err := term.MakeRaw(int(t.in.Fd())); err == nil {
			t.state = st
		}
	}
	if t.out != nil {
		t.out.WriteString("\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	}
	return t
}

func (t *terminal) size() (int, int) {
	if t.out != nil {
		if w, h, err := term.GetSize(int(t.out.Fd())); err == nil && w > 0 && h > 0 {
			return w, h
		}
	}
	return t.fallback[0], t.fallback[1]
}

func (t *terminal) close() {
	if t.out != nil {
		t.out.WriteString("\x1b[?25h\x1b[?1049l") // show cursor, main screen
	}
	if t.state != nil && t.in != nil {
		term.Restore(int(t.in.Fd()), t.state)
	}
}
