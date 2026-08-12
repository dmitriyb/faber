package failure

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Status is the three-word result vocabulary: a step completed its contract
// (ok), did not (failed), or settled decisively while stopping the run for an
// operator (halted). An unfavorable-but-valid payload is ok — workflow
// conditions, not failure semantics, react to it. A halt is not an error:
// nothing broke, so there is nothing to retry or clean up — but no payload
// threads and dependents must not run.
type Status string

// Result statuses.
const (
	StatusOK     Status = "ok"
	StatusFailed Status = "failed"
	StatusHalted Status = "halted"
)

// Stable machine error reasons — a small vocabulary per producer. Detail
// carries the human (or, by per-reason convention, machine-readable JSON)
// elaboration; Reason is what code branches on.
const (
	ReasonEnvContract    = "env-contract"    // context hook broke the env contract
	ReasonHook           = "hook"            // a box hook refused or crashed
	ReasonAgent          = "agent"           // the agent phase died
	ReasonResultSchema   = "result-schema"   // payload failed schema validation / result contract
	ReasonVerify         = "verify"          // result verification failed
	ReasonLaunch         = "launch"          // the container / attempt never launched
	ReasonCanceled       = "canceled"        // the run's context was cancelled before/around the attempt
	ReasonLoopExhausted  = "loop-exhausted"  // a loop selector settled without the until predicate holding
	ReasonSourceContract = "source-contract" // a generate data source broke its contract
)

// Result is the one record every step attempt produces — failure is a record,
// not an absence. Exactly one of Payload/Error/Halt is present, discriminated
// by Status. The same record is simultaneously the threading payload, the
// failure signal, the journal entry, and the metering input.
type Result struct {
	Status Status `json:"status"`
	// Payload is the schema-typed output; it is schema-validated upstream (by
	// the agent module's result extraction) before any consumer sees it.
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *ErrorRecord    `json:"error,omitempty"`
	// Halt is status halted's record arm: the halter's own vocabulary,
	// surfaced verbatim in the report and never branched on by the engine.
	Halt   *HaltRecord `json:"halt,omitempty"`
	Timing Timing      `json:"timing"`
	// Attempt is the 1-based attempt number this record describes.
	Attempt int `json:"attempt"`
	// Attempts is the prior attempts' history, oldest first, carried on the
	// final record of a retry sequence so journal and report show the whole
	// story in one record.
	Attempts []AttemptInfo `json:"attempts,omitempty"`
}

// ErrorRecord is the structured failure description inside a failed Result.
type ErrorRecord struct {
	// Reason is a stable machine word (see the Reason* vocabulary).
	Reason string `json:"reason"`
	// Detail is human text by default; per-reason conventions may carry
	// machine-readable JSON (e.g. a rate-limit reset epoch).
	Detail string `json:"detail"`
	// Handoff is an optional pointer, resolvable under the run directory, to
	// preserved diagnostic state (context bundle, hook outputs, reason file).
	Handoff string `json:"handoff,omitempty"`
}

// HaltRecord is the structured operator-stop description inside a halted
// Result: why the run must not continue past this step.
type HaltRecord struct {
	// Reason is a stable machine word chosen by the halter (a hook or the
	// agent's skill); faber never interprets it.
	Reason string `json:"reason"`
	// Detail is optional human elaboration.
	Detail string `json:"detail,omitempty"`
	// Phase names the box phase that requested the halt (context, prelude,
	// agent, postlude), when the halt came out of a box.
	Phase string `json:"phase,omitempty"`
}

// Timing records one attempt's wall-clock span.
type Timing struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
}

// Duration is the attempt's elapsed wall-clock time.
func (t Timing) Duration() time.Duration { return t.Finished.Sub(t.Started) }

// AttemptInfo is one prior attempt's summary inside a final record.
type AttemptInfo struct {
	Attempt int          `json:"attempt"`
	Timing  Timing       `json:"timing"`
	Error   *ErrorRecord `json:"error"`
}

// Validate enforces the record union: ok ⇒ payload set, error and halt
// absent; failed ⇒ error set (with a reason), payload and halt absent;
// halted ⇒ halt set (with a reason), payload and error absent; attempt ≥ 1.
// Every boundary that accepts a Result (journal append, threading, metering)
// calls it — cheap defense against a hand-edited result.json.
func (r *Result) Validate() error {
	var errs []error
	switch r.Status {
	case StatusOK:
		if len(r.Payload) == 0 {
			errs = append(errs, errors.New("status ok requires a payload"))
		}
		if r.Error != nil {
			errs = append(errs, errors.New("status ok must not carry an error record"))
		}
		if r.Halt != nil {
			errs = append(errs, errors.New("status ok must not carry a halt record"))
		}
	case StatusFailed:
		if r.Error == nil {
			errs = append(errs, errors.New("status failed requires an error record"))
		} else if r.Error.Reason == "" {
			errs = append(errs, errors.New("error record requires a reason"))
		}
		if r.Payload != nil {
			errs = append(errs, errors.New("status failed must not carry a payload"))
		}
		if r.Halt != nil {
			errs = append(errs, errors.New("status failed must not carry a halt record"))
		}
	case StatusHalted:
		if r.Halt == nil {
			errs = append(errs, errors.New("status halted requires a halt record"))
		} else if r.Halt.Reason == "" {
			errs = append(errs, errors.New("halt record requires a reason"))
		}
		if r.Payload != nil {
			errs = append(errs, errors.New("status halted must not carry a payload"))
		}
		if r.Error != nil {
			errs = append(errs, errors.New("status halted must not carry an error record"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown status %q (ok|failed|halted)", r.Status))
	}
	if r.Attempt < 1 {
		errs = append(errs, fmt.Errorf("attempt %d is not 1-based", r.Attempt))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("failure: invalid result record: %w", err)
	}
	return nil
}
