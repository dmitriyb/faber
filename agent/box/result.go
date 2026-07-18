package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/dmitriyb/faber/agent/contract"
)

// emitResult is phase 10: read the skill-written output file, validate it
// against the template's declared output schema (all violations collected),
// verify the bundle's declared side-effects against the gateway, and write
// the single ok attempt record. Every failure returns through the fail-stop
// funnel, which writes the failed record — one record per attempt is an
// invariant of the binary, not a convention.
func (b *Box) emitResult(ctx context.Context) error {
	payload, fallback, err := readOutput(b.Env.ResultDir)
	if err != nil {
		return err
	}
	violations, extras := contract.ValidateOutput(b.Env.Schema, payload)
	if len(violations) > 0 {
		// The fallback with required outputs is a quiet agent, not a shape
		// mismatch: it gets its own reason.
		reason := contract.ReasonOutputSchema
		if fallback {
			reason = contract.ReasonMissingOutput
		}
		return &boxError{Reason: reason, Detail: contract.JoinViolations(violations)}
	}
	if err := b.verifySideEffects(ctx); err != nil {
		return err
	}
	rec := contract.Result{
		Status:     contract.StatusOK,
		Payload:    payload,
		Unthreaded: extras,
		Fallback:   fallback,
		Timing:     b.Timing,
		Attempt:    b.Env.Attempt,
	}
	if err := contract.WriteResultFile(b.Env.ResultDir, rec); err != nil {
		return &boxError{Reason: contract.ReasonResultWrite, Detail: err.Error()}
	}
	b.Log.InfoContext(ctx, "result emitted", "status", rec.Status, "fallback", rec.Fallback)
	return nil
}

// readOutput reads the well-known output file. Absence after a successful
// agent phase is the engine-written fallback: an empty payload with the
// fallback marker — an agent that says nothing does not produce an absent
// step. Unparseable content is an output-schema failure.
func readOutput(resultDir string) (payload map[string]any, fallback bool, err error) {
	raw, rerr := os.ReadFile(filepath.Join(resultDir, contract.OutputFile))
	if rerr != nil {
		if errors.Is(rerr, fs.ErrNotExist) {
			return map[string]any{}, true, nil
		}
		return nil, false, &boxError{Reason: contract.ReasonOutputSchema, Detail: fmt.Sprintf("read %s: %v", contract.OutputFile, rerr)}
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, false, &boxError{
			Reason: contract.ReasonOutputSchema,
			Detail: fmt.Sprintf("%s is not a JSON object: %v", contract.OutputFile, err),
		}
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, false, nil
}

// verifySideEffects checks the bundle's declared postconditions. The one
// first-pass convention: a BRANCH entry with a bound repo means "after this
// step, that branch exists on the gateway" — the payload is the agent's
// claim, the gateway's state is the evidence. Verification runs in-box
// because only the box can reach the gateway.
func (b *Box) verifySideEffects(ctx context.Context) error {
	branch, ok := b.Bundle.Env[contract.BranchKey]
	if !ok {
		return nil
	}
	res, err := b.Runner.Run(ctx, CmdSpec{
		Argv: []string{"git", "ls-remote", "--exit-code", "origin", "refs/heads/" + branch},
		Dir:  b.Workdir,
		Env:  b.Environ,
	})
	if err != nil {
		return &boxError{Reason: contract.ReasonSideEffectUnverified, Detail: fmt.Sprintf("verify branch %q: %v", branch, err)}
	}
	if res.ExitCode != 0 {
		return &boxError{
			Reason:     contract.ReasonSideEffectUnverified,
			Detail:     fmt.Sprintf("declared branch %q does not exist on the gateway", branch),
			ExitCode:   res.ExitCode,
			StderrTail: string(res.StderrTail),
		}
	}
	b.Log.InfoContext(ctx, "side-effect verified", "branch", branch)
	return nil
}
