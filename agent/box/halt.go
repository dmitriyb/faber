package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmitriyb/faber/agent/contract"
)

// haltRequest is the decoded halt file: the halter's reason and optional
// detail. The shape is deliberately minimal — the reason is the user's
// vocabulary and the engine never interprets it.
type haltRequest struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// haltRequested reports whether a halt file exists in the result directory —
// the cheap existence probe the prelude phase uses to exempt a halting
// prelude from the bundle postcondition (no agent will run, so no prompt
// bundle is owed). Content validation stays with checkHalt.
func (b *Box) haltRequested() bool {
	_, err := os.Stat(filepath.Join(b.Env.ResultDir, contract.HaltFile))
	return err == nil
}

// checkHalt honors an operator-stop request after a user phase exited 0: it
// reads the halt file from the result directory (size-bounded — the file is
// box-side user bytes crossing to the host), and when present writes the
// halted attempt record and reports halted=true so Main stops the phase
// order there. A file that exists but does not parse, or names no reason,
// returns a halt-invalid error — an ambiguous stop request fails the step
// loudly instead of guessing. Absence is the common case and a no-op.
func (b *Box) checkHalt(ctx context.Context, phaseName string) (halted bool, err error) {
	raw, rerr := contract.ReadBoundedFile(filepath.Join(b.Env.ResultDir, contract.HaltFile))
	if rerr != nil {
		if errors.Is(rerr, fs.ErrNotExist) {
			return false, nil
		}
		return false, &boxError{Reason: contract.ReasonHaltInvalid, Detail: fmt.Sprintf("read %s: %v", contract.HaltFile, rerr)}
	}
	var req haltRequest
	if jerr := json.Unmarshal(raw, &req); jerr != nil {
		return false, &boxError{Reason: contract.ReasonHaltInvalid, Detail: fmt.Sprintf("%s is not a JSON object: %v", contract.HaltFile, jerr)}
	}
	if strings.TrimSpace(req.Reason) == "" {
		return false, &boxError{Reason: contract.ReasonHaltInvalid, Detail: contract.HaltFile + " names no reason; a halt request must say why"}
	}
	rec := contract.Result{
		Status:  contract.StatusHalted,
		Halt:    &contract.ResultHalt{Reason: req.Reason, Detail: req.Detail, Phase: phaseName},
		Timing:  b.Timing,
		Attempt: b.Env.Attempt,
	}
	if werr := contract.WriteResultFile(b.Env.ResultDir, rec); werr != nil {
		// The record could not land; without it the host would synthesize
		// box-vanished. Fail the phase so the standard funnel reports it.
		return false, &boxError{Reason: contract.ReasonResultWrite, Detail: werr.Error()}
	}
	b.Log.InfoContext(ctx, "halt requested; stopping the phase order",
		"phase", phaseName, "reason", req.Reason)
	return true, nil
}
