package pipeline

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// EventKind names one run progress event.
type EventKind string

const (
	EventStepStarted      EventKind = "step-started"
	EventStepSettled      EventKind = "step-settled"
	EventStepDeferred     EventKind = "step-deferred"
	EventGenerateExpanded EventKind = "generate-expanded"
	EventPauseRequested   EventKind = "pause-requested"
	EventRunEnded         EventKind = "run-ended"
)

// Event is one typed progress event — the run's heartbeat as data rather
// than as a log line. The scheduler emits them at the points that settle,
// start, defer, expand, pause, or end; the executor's sink decides what they
// look like. Fields not meaningful for a kind are zero.
type Event struct {
	Kind EventKind
	Time time.Time
	Step string // node id; empty for run-level events

	// step-started
	Template string
	Attempt  int

	// step-settled
	Status   string // StateOK|StateFailed|StateHalted|skip states
	Reason   string // failure reason or halt reason
	Cached   bool
	Duration time.Duration

	// step-deferred (Until zero = re-check on the next settlement)
	Until  time.Time
	Detail string // defer detail; run-ended detail

	// generate-expanded
	Items int
	Nodes int

	// run-ended
	RunID     string
	RunStatus string // failure.RunEndSettled|Aborted|Paused
	Failed    int
	Halted    int
}

// EventSink receives progress events. Emit is called from the scheduler's
// loop goroutine and from box workers (attempt starts), so implementations
// must be safe for concurrent use. A nil sink is "no events".
type EventSink interface {
	Emit(Event)
}

// LineSink is the minimal human sink: one ASCII line per event, no colour,
// so the stream is visible end to end before a real renderer exists.
type LineSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewLineSink writes one line per event to w.
func NewLineSink(w io.Writer) *LineSink { return &LineSink{w: w} }

// Emit implements EventSink.
func (s *LineSink) Emit(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintln(s.w, formatEventLine(ev))
}

// formatEventLine renders one event as "HH:MM:SS  <word>  <step>  <extra>".
// Box-derived text (reasons, details) is terminal-sanitized exactly like the
// report's Text path.
func formatEventLine(ev Event) string {
	ts := ev.Time.Format("15:04:05")
	switch ev.Kind {
	case EventStepStarted:
		extra := ""
		if ev.Attempt > 1 {
			extra = fmt.Sprintf("  attempt %d", ev.Attempt)
		}
		return fmt.Sprintf("%s  start   %s%s", ts, ev.Step, extra)
	case EventStepSettled:
		word := settleWord(ev.Status)
		var sb []byte
		sb = fmt.Appendf(sb, "%s  %-7s %s", ts, word, ev.Step)
		if ev.Duration > 0 {
			sb = fmt.Appendf(sb, "  %s", ev.Duration.Round(time.Second))
		}
		if ev.Cached {
			sb = fmt.Appendf(sb, "  (cached)")
		}
		if ev.Reason != "" {
			sb = fmt.Appendf(sb, "  %s", sanitizeTerm(ev.Reason))
		}
		return string(sb)
	case EventStepDeferred:
		if ev.Until.IsZero() {
			return fmt.Sprintf("%s  WAIT    %s  until next settlement  %s", ts, ev.Step, sanitizeTerm(ev.Detail))
		}
		return fmt.Sprintf("%s  WAIT    %s  until %s  %s", ts, ev.Step, ev.Until.Format("15:04:05"), sanitizeTerm(ev.Detail))
	case EventGenerateExpanded:
		return fmt.Sprintf("%s  expand  %s  %d items, %d nodes", ts, ev.Step, ev.Items, ev.Nodes)
	case EventPauseRequested:
		return fmt.Sprintf("%s  PAUSE   run: in-flight steps settle, nothing new dispatches", ts)
	case EventRunEnded:
		var sb []byte
		sb = fmt.Appendf(sb, "%s  end     run %s: %s", ts, ev.RunID, ev.RunStatus)
		if ev.Failed > 0 {
			sb = fmt.Appendf(sb, ", %d failed", ev.Failed)
		}
		if ev.Halted > 0 {
			sb = fmt.Appendf(sb, ", %d halted", ev.Halted)
		}
		if ev.Detail != "" {
			sb = fmt.Appendf(sb, "  %s", sanitizeTerm(ev.Detail))
		}
		return string(sb)
	}
	return fmt.Sprintf("%s  %s  %s", ts, ev.Kind, ev.Step)
}

// settleWord maps a terminal state onto the stream's status vocabulary:
// lower-case for the routine, upper-case for what needs the eye.
func settleWord(status string) string {
	switch status {
	case StateOK:
		return "ok"
	case StateFailed:
		return "FAIL"
	case StateHalted:
		return "HALT"
	case StateSkippedCondition:
		return "skip"
	case StateSkippedDependency, StateSkippedHalt:
		return "skip"
	}
	return status
}
