package failure

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"

	"github.com/dmitriyb/faber/config"
)

// RunSeed is a recovery mode's seeding of the scheduler: the run header, the
// prior results to consult at readiness time, and the (re)opened journal to
// append new settlements to. Resume seeds Prior from the replayed journal;
// fresh seeds it empty.
type RunSeed struct {
	Header Header
	// Prior is the last-wins (step-id, input-hash) → result map replayed from
	// the journal. Empty for fresh runs.
	Prior   map[Key]ResultRecord
	Journal *Journal
}

// Resume re-enters a stopped run. The journal header is checked first: the
// caller re-derives the IR from the header's config (the config pipeline is
// deterministic), and the IR hash and supplied params must match the header —
// a mismatch means the graph changed and journal keys cannot be trusted, so
// Resume refuses and points at fresh (--no-cache). On a match it returns a
// seed whose Prior map the scheduler consults at readiness time.
func (s *Store) Resume(ir *config.IR, runID string, supplied map[string]string) (*RunSeed, error) {
	rp, err := s.Load(runID)
	if err != nil {
		return nil, err
	}
	hash, err := config.HashIR(ir)
	if err != nil {
		return nil, fmt.Errorf("failure: resume %s: %w", runID, err)
	}
	if hash != rp.Header.IRHash {
		return nil, fmt.Errorf(
			"failure: resume %s: config drift: IR hash mismatch (journal %s, current %s) — journal keys cannot be trusted; use --no-cache to start fresh",
			runID, rp.Header.IRHash, hash)
	}
	if !maps.Equal(nonNil(supplied), nonNil(rp.Header.Params)) {
		return nil, fmt.Errorf(
			"failure: resume %s: config drift: supplied params differ from the journaled run's — journal keys cannot be trusted; use --no-cache to start fresh",
			runID)
	}
	j, err := s.Reopen(runID)
	if err != nil {
		return nil, err
	}
	return &RunSeed{Header: rp.Header, Prior: rp.Results, Journal: j}, nil
}

// Fresh starts a brand-new run that ignores any prior journal (--no-cache):
// a new run directory and journal under hdr.RunID, an empty prior map, no
// lookups. Old journals are left untouched as records of abandoned runs.
func (s *Store) Fresh(hdr Header) (*RunSeed, error) {
	j, err := s.Begin(hdr)
	if err != nil {
		return nil, err
	}
	return &RunSeed{Header: hdr, Prior: map[Key]ResultRecord{}, Journal: j}, nil
}

// Lookup is the readiness-time skip decision: the step's input-hash is
// computed from its just-resolved inputs, template identity, and image tag,
// and a journaled ok record under the matching key is returned for reuse —
// its payload threads to dependents exactly as if the step had just run. A
// failed or absent record means the step runs. There is no upfront skip set:
// input-hashes depend on upstream payloads, so this is answerable only when
// the step becomes ready.
func (rs *RunSeed) Lookup(stepID string, inputs map[string]any, template, imageTag string) (Result, bool, error) {
	h, err := InputHash(inputs, template, imageTag)
	if err != nil {
		return Result{}, false, fmt.Errorf("failure: lookup %s: %w", stepID, err)
	}
	rec, ok := rs.Prior[Key{StepID: stepID, InputHash: h}]
	if !ok || rec.Result.Status != StatusOK {
		return Result{}, false, nil // failed or absent ⇒ run it
	}
	return rec.Result, true, nil
}

// InteractiveTarget is everything the box-reconstruction seam needs to
// rebuild the failed step's box for an operator shell: the run directory
// (under which the record's handoff pointer resolves), the run header, and
// the failed step's final result record. It deliberately carries no journal
// handle — an interactive session is observation, not execution.
type InteractiveTarget struct {
	RunDir string
	Header Header
	StepID string
	Record ResultRecord
}

// HandoffPath resolves the failed record's handoff pointer under the run
// directory. ok is false when the record preserved no handoff state.
func (t InteractiveTarget) HandoffPath() (string, bool) {
	if t.Record.Result.Error == nil || t.Record.Result.Error.Handoff == "" {
		return "", false
	}
	return filepath.Join(t.RunDir, t.Record.Result.Error.Handoff), true
}

// BoxReentry reconstructs a failed step's box — same image tag, same security
// bindings, same resolved inputs exported as the step env — with the entry
// program replaced by an interactive shell and the handoff directory
// surfaced read-only. It is implemented by the executor (over the infra and
// security modules) at integration time and is fakeable in tests.
type BoxReentry interface {
	Reenter(ctx context.Context, t InteractiveTarget) error
}

// Interactive re-enters a run at a failed step for manual diagnosis. It
// refuses unless the step's last journaled record is failed, naming the
// step's actual state otherwise. Nothing is appended to the journal — the
// run's state is unchanged afterward; the operator then chooses resume or
// fresh.
func (s *Store) Interactive(ctx context.Context, runID, stepID string, re BoxReentry) error {
	rp, err := s.Load(runID)
	if err != nil {
		return err
	}
	rec, ok := rp.LastByStep[stepID]
	if !ok {
		return fmt.Errorf(
			"failure: interactive: run %s has no journal record for step %s (it never settled); resume will run it normally",
			runID, stepID)
	}
	if rec.Result.Status != StatusFailed {
		return fmt.Errorf(
			"failure: interactive: step %s settled %q in run %s; interactive re-entry is only for failed steps",
			stepID, rec.Result.Status, runID)
	}
	return re.Reenter(ctx, InteractiveTarget{
		RunDir: s.RunDir(runID),
		Header: rp.Header,
		StepID: stepID,
		Record: rec,
	})
}

// nonNil normalizes a nil map to empty for comparison.
func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
