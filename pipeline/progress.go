package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TextSink is the human progress renderer: one ASCII line per event that
// matters to an operator, with the node id folded into a breadcrumb that
// carries iteration and cap ("cycles 8/45 > review-cycle 1/5 > review"),
// an aligned status column, and the scheduler's skip settlements collapsed
// into what they mean — "not needed" for a guard inside a live iteration,
// one "settled after n of cap" line when a loop converges, one "not reached"
// range line per loop past a failure. Colour, when on, touches the status
// word only and uses the 16 palette slots, so the terminal theme owns it.
type TextSink struct {
	mu    sync.Mutex
	w     io.Writer
	color bool

	workflow string
	bounds   map[string]int  // loop prefix -> cap
	expected map[string]int  // loop prefix -> the expect command's last count
	started  map[string]bool // iteration prefixes ("<loop>@<i>/") with a started node
	maxIter  map[string]int  // loop prefix -> highest iteration that started
	settled  map[string]bool // loops whose convergence line was printed
	pending  map[string]*nrRange
	order    []string
}

// nrRange accumulates one loop's not-reached iterations until run end.
type nrRange struct {
	lo, hi   int
	ancestor string
}

// NewTextSink renders to w; color turns the status-word colouring on.
func NewTextSink(w io.Writer, color bool) *TextSink {
	return &TextSink{
		w: w, color: color,
		bounds: map[string]int{}, expected: map[string]int{}, started: map[string]bool{}, maxIter: map[string]int{},
		settled: map[string]bool{}, pending: map[string]*nrRange{},
	}
}

// Emit implements EventSink.
func (s *TextSink) Emit(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines(ev) {
		fmt.Fprintln(s.w, l)
	}
}

// status words of the progress stream
const (
	wordStart  = "start"
	wordWait   = "WAIT"
	wordExpand = "expand"
	wordRun    = "run"
	wordEnd    = "end"
	wordPause  = "PAUSE"
	wordReady  = "ready"
	wordExpect = "expect"
)

// sgr is the status-word colour map: palette slots only, never 256-colour.
var sgr = map[string]string{
	wordOK: "32", wordFail: "1;31", wordHalt: "1;31", wordStart: "36", wordWait: "33",
	wordNotNeeded: "90", wordNotReached: "90", wordPause: "33", wordReady: "33",
}

func (s *TextSink) word(w string) string {
	padded := fmt.Sprintf("%-12s", w)
	if code, ok := sgr[w]; ok && s.color {
		return "\x1b[" + code + "m" + padded + "\x1b[0m"
	}
	return padded
}

func (s *TextSink) line(ev Event, word, rest string) string {
	return fmt.Sprintf("%s  %s %s", ev.Time.Format("15:04:05"), s.word(word), rest)
}

func (s *TextSink) lines(ev Event) []string {
	switch ev.Kind {
	case EventRunStarted:
		s.workflow = ev.Workflow
		for k, v := range ev.Loops {
			s.bounds[k] = v
		}
		return []string{s.line(ev, wordRun, fmt.Sprintf("%s  workflow %s", ev.RunID, ev.Workflow))}
	case EventStepStarted:
		s.noteStart(ev.Step)
		extra := ""
		if ev.Attempt > 1 {
			extra = fmt.Sprintf("  attempt %d", ev.Attempt)
		}
		return []string{s.line(ev, wordStart, s.crumb(ev.Step)+extra)}
	case EventStepSettled:
		return s.settledLines(ev)
	case EventLoopExpected:
		s.expected[ev.Step] = ev.Expected
		return []string{s.line(ev, wordExpect, fmt.Sprintf("%s  %d of up to %d", s.crumb(ev.Step), ev.Expected, ev.Cap))}
	case EventStepWaiting:
		return []string{s.line(ev, wordReady, fmt.Sprintf("%s  waiting for %s", s.crumb(ev.Step), sanitizeTerm(ev.Detail)))}
	case EventStepDeferred:
		when := "until next settlement"
		if !ev.Until.IsZero() {
			when = "until " + ev.Until.Format("15:04:05")
		}
		return []string{s.line(ev, wordWait, fmt.Sprintf("%s  %s  %s", s.crumb(ev.Step), when, sanitizeTerm(ev.Detail)))}
	case EventGenerateExpanded:
		for k, v := range ev.Loops {
			s.bounds[k] = v
		}
		return []string{s.line(ev, wordExpand, fmt.Sprintf("%s  %d items", s.crumb(ev.Step), ev.Items))}
	case EventPauseRequested:
		return []string{s.line(ev, wordPause, "in-flight steps settle, nothing new dispatches")}
	case EventRunEnded:
		out := s.flush(ev)
		rest := fmt.Sprintf("run %s: %s", ev.RunID, ev.RunStatus)
		if ev.Failed > 0 {
			rest += fmt.Sprintf(", %d failed", ev.Failed)
		}
		if ev.Halted > 0 {
			rest += fmt.Sprintf(", %d halted", ev.Halted)
		}
		if ev.Detail != "" {
			rest += "  " + sanitizeTerm(ev.Detail)
		}
		return append(out, s.line(ev, wordEnd, rest))
	}
	return nil
}

// settledLines turns one settlement into zero, one, or two lines.
func (s *TextSink) settledLines(ev Event) []string {
	if executed(ev.Status) {
		var sb strings.Builder
		sb.WriteString(s.crumb(ev.Step))
		if ev.Duration > 0 {
			fmt.Fprintf(&sb, "  %s", ev.Duration.Round(time.Second))
		}
		if ev.Cached {
			sb.WriteString("  (cached)")
		}
		if ev.Reason != "" {
			sb.WriteString("  " + sanitizeTerm(ev.Reason))
		}
		return []string{s.line(ev, statusWord(ev.Status), sb.String())}
	}
	loop, idle := s.outermostIdle(ev.Step)
	switch ev.Status {
	case StateSkippedCondition:
		if !idle {
			return []string{s.line(ev, wordNotNeeded, s.crumb(ev.Step))}
		}
		if s.settled[loop] {
			return nil
		}
		s.settled[loop] = true
		if n := s.maxIter[loop]; n > 0 {
			return []string{s.line(ev, wordOK, fmt.Sprintf("%s  settled after %d of %d", s.crumb(loop), n, s.bounds[loop]))}
		}
		return []string{s.line(ev, wordNotNeeded, s.crumb(loop))}
	case StateSkippedDependency, StateSkippedHalt:
		anc := s.crumb(ev.Reason)
		if !idle {
			return []string{s.line(ev, wordNotReached, fmt.Sprintf("%s  after %s", s.crumb(ev.Step), anc))}
		}
		_, i, _ := loopOf(ev.Step, loopScope(loop))
		r := s.pending[loop]
		if r == nil {
			r = &nrRange{lo: i, hi: i, ancestor: anc}
			s.pending[loop] = r
			s.order = append(s.order, loop)
		}
		if i < r.lo {
			r.lo = i
		}
		if i > r.hi {
			r.hi = i
		}
		return nil
	}
	return []string{s.line(ev, statusWord(ev.Status), s.crumb(ev.Step))}
}

// flush renders the accumulated not-reached ranges, in first-seen order.
func (s *TextSink) flush(ev Event) []string {
	var out []string
	for _, loop := range s.order {
		r := s.pending[loop]
		rest := s.crumb(loop)
		if s.maxIter[loop] > 0 {
			if r.lo == r.hi {
				rest += fmt.Sprintf(" @%d", r.lo)
			} else {
				rest += fmt.Sprintf(" @%d..@%d", r.lo, r.hi)
			}
		}
		out = append(out, s.line(ev, wordNotReached, rest+"  after "+r.ancestor))
	}
	s.pending = map[string]*nrRange{}
	s.order = nil
	return out
}

// noteStart records which iteration prefixes a started node sits in.
func (s *TextSink) noteStart(id string) {
	segs := strings.Split(id, "/")
	for k, seg := range segs {
		if name, i, ok := parseIter(seg); ok {
			loop := strings.Join(append(append([]string{}, segs[:k]...), name), "/")
			s.started[loop+"@"+strconv.Itoa(i)+"/"] = true
			if i > s.maxIter[loop] {
				s.maxIter[loop] = i
			}
		}
	}
}

// outermostIdle finds the outermost enclosing loop whose iteration never
// started a node — the loop a skip collapses into. idle is false when every
// enclosing iteration is live (or there is none).
func (s *TextSink) outermostIdle(id string) (loop string, idle bool) {
	segs := strings.Split(id, "/")
	for k, seg := range segs {
		if name, i, ok := parseIter(seg); ok {
			loop = strings.Join(append(append([]string{}, segs[:k]...), name), "/")
			if !s.started[loop+"@"+strconv.Itoa(i)+"/"] {
				return loop, true
			}
		}
	}
	return "", false
}

// loopScope is the scope under which loopOf parses a loop's own segment:
// everything before the loop's last path segment.
func loopScope(loop string) string {
	if i := strings.LastIndex(loop, "/"); i >= 0 {
		return loop[:i+1]
	}
	return ""
}

// crumb folds a node id into the breadcrumb: the workflow root segment
// dropped, iteration segments as "name i/cap", segments joined by " > ".
// Loop caps are keyed by the full prefix id, so the key is built from the
// original segments even when the root is not rendered.
func (s *TextSink) crumb(id string) string {
	if id == "" {
		return ""
	}
	segs := strings.Split(id, "/")
	first := 0
	if len(segs) > 1 && segs[0] == s.workflow {
		first = 1
	}
	parts := make([]string, 0, len(segs))
	for k := first; k < len(segs); k++ {
		seg := segs[k]
		if name, i, ok := parseIter(seg); ok {
			loop := strings.Join(append(append([]string{}, segs[:k]...), name), "/")
			if exp := s.expected[loop]; exp > 0 {
				parts = append(parts, fmt.Sprintf("%s %d/%d (cap %d)", name, i, exp, s.bounds[loop]))
			} else if cap := s.bounds[loop]; cap > 0 {
				parts = append(parts, fmt.Sprintf("%s %d/%d", name, i, cap))
			} else {
				parts = append(parts, fmt.Sprintf("%s %d", name, i))
			}
			continue
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, " > ")
}

// JSONSink writes one JSON object per event — the machine form of the
// progress stream, for a non-TTY stderr or an explicit --progress json.
type JSONSink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewJSONSink writes JSON lines to w.
func NewJSONSink(w io.Writer) *JSONSink {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &JSONSink{enc: enc}
}

// Emit implements EventSink.
func (s *JSONSink) Emit(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.enc.Encode(ev)
}
