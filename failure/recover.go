package failure

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"strings"

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
	Prior map[Key]ResultRecord
	// Costs is every journaled cost record replayed from the prior run, in
	// journal order — the material the executor folds into the metering
	// admitter's spent ledger so a declared budget covers the whole logical
	// run across interruptions. Empty for fresh runs.
	Costs   []CostRecord
	Journal *Journal
}

// Resume re-enters a stopped run. Exclusivity comes first: the run's
// advisory lock is acquired before any read, so a live run refuses loudly
// and replay never races a live appender. Then the journal header is
// checked: the caller re-derives the IR from the header's config (the config
// pipeline is deterministic), and the IR hash and supplied params must match
// the header — a mismatch means the graph changed and journal keys cannot be
// trusted, so Resume refuses and points at fresh (--fresh). On a match it
// returns a seed whose Prior map the scheduler consults at readiness time.
func (s *Store) Resume(ir *config.IR, runID string, supplied map[string]string) (*RunSeed, error) {
	lock, err := AcquireRunLock(s.RunDir(runID))
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*RunSeed, error) {
		lock.Release()
		return nil, err
	}
	rp, err := s.Load(runID)
	if err != nil {
		return fail(err)
	}
	hash, err := config.HashIR(ir)
	if err != nil {
		return fail(fmt.Errorf("failure: resume %s: %w", runID, err))
	}
	if rp.Header.IRVersion != 0 && rp.Header.IRVersion != ir.IRVersion {
		// The IR schema itself moved between the journaling faber and this
		// one — an engine upgrade, not operator config drift; the messages
		// must not blame the config.
		return fail(fmt.Errorf(
			"failure: resume %s: faber's IR schema changed since this run was journaled (journal IR v%d, current v%d); no auto-migration — finish the run on the faber that wrote it, or start over with --fresh",
			runID, rp.Header.IRVersion, ir.IRVersion))
	}
	if hash != rp.Header.IRHash {
		return fail(fmt.Errorf(
			"failure: resume %s: config drift: IR hash mismatch (journal %s, current %s) — journal keys cannot be trusted; use --fresh to start over",
			runID, rp.Header.IRHash, hash))
	}
	if !maps.Equal(nonNil(supplied), nonNil(rp.Header.Params)) {
		return fail(fmt.Errorf(
			"failure: resume %s: config drift: supplied params differ from the journaled run's — journal keys cannot be trusted; use --fresh to start over",
			runID))
	}
	j, err := s.reopenLocked(runID, lock)
	if err != nil {
		return fail(err)
	}
	return &RunSeed{Header: rp.Header, Prior: rp.Results, Costs: rp.Costs, Journal: j}, nil
}

// Fresh starts a brand-new run that ignores any prior journal (--fresh):
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
// directory. ok is false when the record preserved no handoff state — or
// when the journaled pointer does not stay under the run directory: the
// pointer's parent is bind-mounted into the operator's re-entry container,
// so a pointer that cleans to an escape (a hand-edited or pre-hardening
// journal) must never resolve.
func (t InteractiveTarget) HandoffPath() (string, bool) {
	if t.Record.Result.Error == nil || t.Record.Result.Error.Handoff == "" {
		return "", false
	}
	joined := filepath.Join(t.RunDir, t.Record.Result.Error.Handoff)
	base := filepath.Clean(t.RunDir)
	if !strings.HasPrefix(joined, base+string(filepath.Separator)) {
		return "", false
	}
	return joined, true
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
	// Skip settlements and other cheap failures journal with an empty input
	// hash (the forgery-resistant skip encoding) — no box ever ran, so there
	// is nothing to re-enter. Refusing here keeps a skip record from passing
	// the failed-status gate above.
	if rec.InputHash == "" {
		return fmt.Errorf(
			"failure: interactive: step %s settled without executing a box in run %s (a skip or launch-stage failure); there is no box state to re-enter",
			stepID, runID)
	}
	if rec.Result.Error == nil || rec.Result.Error.Handoff == "" {
		return fmt.Errorf(
			"failure: interactive: step %s preserved no handoff state in run %s; re-run the step instead",
			stepID, runID)
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
