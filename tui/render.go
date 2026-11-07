package tui

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The role table: one palette slot per role, nothing else coloured, so the
// terminal theme owns every colour and a colour-stripped frame loses
// nothing. Chrome (titles, key letters) bold magenta; identifiers blue;
// the status column by meaning; tertiary bright black; values the default
// foreground.
const (
	sgrReset  = "\x1b[0m"
	sgrChrome = "\x1b[1;35m"
	sgrIdent  = "\x1b[34m"
	sgrDim    = "\x1b[90m"
	sgrRev    = "\x1b[7m"
	sgrSel    = "\x1b[7;34m" // the selection bar: reverse video in the identifier slot
	sgrGreen  = "\x1b[32m"
	sgrRed    = "\x1b[1;31m"
	sgrCyan   = "\x1b[36m"
	sgrYellow = "\x1b[33m"
)

var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// stripSGR removes colour codes — what the golden tests compare.
func stripSGR(s string) string { return sgrPattern.ReplaceAllString(s, "") }

// vis is a string's visible width in cells: ASCII only by construction, so
// one rune is one cell; colour codes count zero.
func vis(s string) int { return utf8.RuneCountInString(stripSGR(s)) }

func pad(s string, n int) string {
	if d := n - vis(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func rpad(s string, n int) string {
	if d := n - vis(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// trunc cuts a plain (uncoloured) string to n cells.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	if n == 1 {
		return string(r[:1])
	}
	return string(r[:n-1]) + "~"
}

// style applies the role table, or nothing when colour is off.
type style struct{ color bool }

func (st style) wrap(code, s string) string {
	if !st.color || s == "" {
		return s
	}
	return code + s + sgrReset
}
func (st style) chrome(s string) string { return st.wrap(sgrChrome, s) }
func (st style) ident(s string) string  { return st.wrap(sgrIdent, s) }
func (st style) dim(s string) string    { return st.wrap(sgrDim, s) }
func (st style) rev(s string) string    { return st.wrap(sgrSel, s) }

// status colours a status word by meaning: the vocabulary of the report
// and the progress stream, nothing invented here.
func (st style) status(word string) string {
	switch word {
	case "ok":
		return st.wrap(sgrGreen, word)
	case "FAIL", "HALT", "FAILED", "MISSING":
		return st.wrap(sgrRed, word)
	case "running", "LIVE", "start":
		return st.wrap(sgrCyan, word)
	case "ready", "WAIT", "PAUSE", "paused", "slow":
		return st.wrap(sgrYellow, word)
	case "not needed", "not reached", "absent", "waiting", "incomplete", "aborted", "not checked":
		return st.wrap(sgrDim, word)
	}
	return word
}

// frame is one full screen: h rows of exactly w cells, each row a string
// that may carry colour codes. Rendering a screen means filling rows; the
// terminal gets the whole frame in one write.
type frame struct {
	w, h int
	rows []string
}

func newFrame(w, h int) *frame {
	return &frame{w: w, h: h, rows: make([]string, h)}
}

// put writes a row (truncated by visible width; padded on output).
func (f *frame) put(y int, s string) {
	if y < 0 || y >= f.h {
		return
	}
	f.rows[y] = fitRow(s, f.w)
}

// fitRow truncates a possibly coloured row to w cells, keeping codes
// balanced by closing with a reset when any code was emitted.
func fitRow(s string, w int) string {
	if vis(s) <= w {
		return s
	}
	var sb strings.Builder
	cells := 0
	colored := false
	for i := 0; i < len(s); {
		if loc := sgrPattern.FindStringIndex(s[i:]); loc != nil && loc[0] == 0 {
			sb.WriteString(s[i : i+loc[1]])
			colored = true
			i += loc[1]
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if cells >= w {
			break
		}
		sb.WriteRune(r)
		cells++
		i += size
	}
	if colored {
		sb.WriteString(sgrReset)
	}
	return sb.String()
}

// bytes renders the frame for the terminal: home the cursor, every row
// padded to width and cleared to the end, rows joined with CR LF.
func (f *frame) bytes() []byte {
	var sb strings.Builder
	sb.WriteString("\x1b[H")
	for i, row := range f.rows {
		sb.WriteString(pad(row, f.w))
		sb.WriteString("\x1b[K")
		if i < len(f.rows)-1 {
			sb.WriteString("\r\n")
		}
	}
	return []byte(sb.String())
}

// plain is the colour-stripped frame as text, one row per line, trailing
// spaces trimmed — the golden form.
func (f *frame) plain() string {
	var sb strings.Builder
	for _, row := range f.rows {
		sb.WriteString(strings.TrimRight(stripSGR(row), " "))
		sb.WriteString("\n")
	}
	return sb.String()
}

// rule is a dim horizontal rule of n cells.
func (st style) rule(n int) string { return st.dim(strings.Repeat("-", max(0, n))) }

// cols lays fixed-width cells on one row, each cell padded to its width.
// A plain cell is truncated to fit; a coloured cell (a status word, an
// identifier) is short by construction and only padded, so colour never
// changes where text falls.
func cols(cells []string, widths []int) string {
	var sb strings.Builder
	for i, c := range cells {
		w := widths[i]
		if i == len(cells)-1 {
			sb.WriteString(c)
			continue
		}
		if !strings.Contains(c, "\x1b[") {
			c = trunc(c, w-1)
		}
		sb.WriteString(pad(c, w))
	}
	return sb.String()
}

// panes joins columns of rows side by side with a dim separator.
func panes(st style, columns [][]string, widths []int, h int) []string {
	out := make([]string, h)
	sep := " " + st.dim("|") + " "
	for y := 0; y < h; y++ {
		var sb strings.Builder
		for c, col := range columns {
			row := ""
			if y < len(col) {
				row = col[y]
			}
			if c > 0 {
				sb.WriteString(sep)
			}
			sb.WriteString(pad(fitRow(row, widths[c]), widths[c]))
		}
		out[y] = sb.String()
	}
	return out
}

// durText renders a duration the way the report does: 2m14s, 1h02m05s.
func durText(secs int) string {
	if secs < 0 {
		secs = 0
	}
	h, m, s := secs/3600, secs%3600/60, secs%60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	}
	return fmt.Sprintf("%dm%02ds", m, s)
}
