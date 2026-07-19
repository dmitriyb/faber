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
		// build, so this detects a stale or foreign FABER_BOX_BIN — the record
		// must not be interpreted as if it spoke this contract.
		return Result{
			Status: StatusFailed,
			Error: &ResultError{
				Reason: contract.ReasonContractVersion,
				Detail: fmt.Sprintf("result record carries contract v%d, host speaks v%d — check FABER_BOX_BIN (a mismatched faber-box binary)", rec.Contract, contract.ContractVersion),
			},
			Timing:  rec.Timing,
			Attempt: rec.Attempt,
		}, nil
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
			Timing:  rec.Timing,
			Attempt: rec.Attempt,
		}, nil
	}
	// Recompute the unthreaded set host-side rather than trusting the box's.
	rec.Unthreaded = extras
	return rec, nil
}
