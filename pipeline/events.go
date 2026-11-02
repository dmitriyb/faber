package pipeline

import (
	"time"
)

// EventKind names one run progress event.
type EventKind string

const (
	EventRunStarted       EventKind = "run-started"
	EventStepStarted      EventKind = "step-started"
	EventStepSettled      EventKind = "step-settled"
	EventStepDeferred     EventKind = "step-deferred"
	EventStepWaiting      EventKind = "step-waiting"
	EventGenerateExpanded EventKind = "generate-expanded"
	EventPauseRequested   EventKind = "pause-requested"
	EventRunEnded         EventKind = "run-ended"
)

// Event is one typed progress event — the run's heartbeat as data rather
// than as a log line. The scheduler emits them at the points that settle,
// start, defer, expand, pause, or end; the executor's sink decides what they
// look like. Fields not meaningful for a kind are zero.
type Event struct {
	Kind EventKind `json:"kind"`
	Time time.Time `json:"time"`
	Step string    `json:"step,omitempty"` // node id; empty for run-level events

	// step-started
	Template string `json:"template,omitempty"`
	Attempt  int    `json:"attempt,omitempty"`

	// step-settled
	Status   string        `json:"status,omitempty"` // StateOK|StateFailed|StateHalted|skip states
	Reason   string        `json:"reason,omitempty"` // failure reason, halt reason, or a skip's ancestor
	Cached   bool          `json:"cached,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`

	// step-deferred (Until zero = re-check on the next settlement)
	Until  time.Time `json:"until,omitempty"`
	Detail string    `json:"detail,omitempty"` // defer detail; step-waiting hold reason; run-ended detail

	// generate-expanded
	Items int `json:"items,omitempty"`
	Nodes int `json:"nodes,omitempty"`

	// run-started, run-ended
	RunID     string `json:"run_id,omitempty"`
	Workflow  string `json:"workflow,omitempty"`
	RunStatus string `json:"run_status,omitempty"` // failure.RunEndSettled|Aborted|Paused
	Failed    int    `json:"failed,omitempty"`
	Halted    int    `json:"halted,omitempty"`

	// Loops carries loop caps a renderer needs to say "8/45": every loop
	// the IR unrolls, on run-started; the instance loops a generate splices
	// in, on generate-expanded. Keyed by loop prefix id.
	Loops map[string]int `json:"loops,omitempty"`
}

// EventSink receives progress events. Emit is called from the scheduler's
// loop goroutine and from box workers (attempt starts), so implementations
// must be safe for concurrent use. A nil sink is "no events".
type EventSink interface {
	Emit(Event)
}
