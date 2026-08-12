package agent

import (
	"fmt"

	"github.com/dmitriyb/faber/agent/contract"
)

// ExtractResult is the host boundary: after the container exits, re-parse
// the attempt record from the mounted result directory and re-validate the
// payload against the template's declared output schema before the record
// reaches threading, the journal, or the meter.
//
// The box is untrusted — a compromised agent can forge its own record — and
// re-validation bounds the damage to mis-shaped data: a payload that fails
// the schema host-side becomes a failed record the pipeline never threads.
// A missing or unparseable record (the sequencer itself was killed) is
// synthesized as a box-vanished failure: no path yields zero records.
func ExtractResult(dir string, schema OutputSchema) (Result, error) {
	if dir == "" {
		return Result{}, fmt.Errorf("agent: extract result: empty result dir")
	}
	rec, err := contract.ReadResultFile(dir)
	if err != nil {
		return Result{
			Status: StatusFailed,
			Error: &ResultError{
				Reason: contract.ReasonBoxVanished,
				Detail: fmt.Sprintf("no readable attempt record: %v", err),
			},
		}, nil
	}
	if rec.Contract != contract.ContractVersion {
		// The record's stamped vintage disagrees with this host (0 = a writer
		// that predates stamping). faber-box ships from the host as the same
		// build, so this detects a stale or foreign box binary (host-config box_bin) — the record
		// must not be interpreted as if it spoke this contract.
		return Result{
			Status: StatusFailed,
			Error: &ResultError{
				Reason: contract.ReasonContractVersion,
				Detail: fmt.Sprintf("result record carries contract v%d, host speaks v%d — check the host config's box_bin (a mismatched faber-box binary)", rec.Contract, contract.ContractVersion),
			},
			AgentSkipped: rec.AgentSkipped,
			Timing:       rec.Timing,
			Attempt:      rec.Attempt,
		}, nil
	}
	if rec.Status == StatusHalted {
		// A halted record threads nothing and skips output validation (the
		// step's declared outputs are moot — dependents will be skipped).
		// The halt arm is checked, not trusted: a box claiming halted
		// without saying why is an invalid record, not an operator-stop.
		rec.Payload = nil
		if rec.Halt == nil || rec.Halt.Reason == "" {
			return Result{
				Status: StatusFailed,
				Error: &ResultError{
					Reason: contract.ReasonHaltInvalid,
					Detail: "halted record without a halt reason",
				},
				Timing:  rec.Timing,
				Attempt: rec.Attempt,
			}, nil
		}
		return rec, nil
	}
	if rec.Status != StatusOK {
		// Already a failure record; never thread its payload.
		rec.Payload = nil
		return rec, nil
	}
	violations, extras := contract.ValidateOutput(schema, rec.Payload)
	if len(violations) > 0 {
		return Result{
			Status: StatusFailed,
			Error: &ResultError{
				Reason: contract.ReasonOutputSchema,
				Detail: "host re-validation: " + contract.JoinViolations(violations),
			},
			AgentSkipped: rec.AgentSkipped,
			Timing:       rec.Timing,
			Attempt:      rec.Attempt,
		}, nil
	}
	// Recompute the unthreaded set host-side rather than trusting the box's.
	rec.Unthreaded = extras
	return rec, nil
}
