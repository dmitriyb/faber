package contract

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Attempt record statuses. Failure is a record, not an absence: every exit
// path of the box lands in exactly one Result with one of these.
const (
	StatusOK     = "ok"
	StatusFailed = "failed"
)

// Failure reasons carried in ResultError.Reason and Handoff.Reason. Hook
// exit, missing bundle, agent crash, schema violation and unverified
// side-effect all converge on the same record shapes and differ only here.
const (
	ReasonEnvContract          = "env-contract"
	ReasonSecrets              = "secrets"
	ReasonHostKeyPolicy        = "host-key-policy"
	ReasonCloneFailed          = "clone-failed"
	ReasonSigning              = "signing"
	ReasonHookFailed           = "hook-failed"
	ReasonBundleMissing        = "bundle-missing"
	ReasonBundleMalformed      = "bundle-malformed"
	ReasonAgentFailed          = "agent-failed"
	ReasonOutputSchema         = "output-schema"
	ReasonMissingOutput        = "missing-output"
	ReasonSideEffectUnverified = "side-effect-unverified"
	ReasonResultWrite          = "result-write"
	ReasonBoxVanished          = "box-vanished"
	ReasonContractVersion      = "contract-version"
)

// HandoffKeyingSlot marks a handoff record whose Inputs map is keyed by
// declared slot names (the shape interactive re-entry consumes directly).
// An absent Keying marks a pre-versioning record keyed by env tokens.
const HandoffKeyingSlot = "slot"

// Result is one step attempt's record: the shape of result.json in the
// mounted result directory and the value the host threads onward. The
// pipeline executor adapts it to the failure module's step-result type at
// integration.
type Result struct {
	// Status is StatusOK or StatusFailed.
	Status string `json:"status"`

	// Contract is the ContractVersion of the writer. WriteResultFile stamps
	// it; the host asserts it on extract, so a record written by a
	// mismatched faber-box binary is surfaced instead of misread.
	Contract int `json:"contract,omitempty"`

	// Payload holds the skill's declared output fields (plus preserved
	// undeclared extras) on an ok attempt.
	Payload map[string]any `json:"payload,omitempty"`

	// Unthreaded names the undeclared extra payload fields: preserved in the
	// record, never visible to downstream wiring.
	Unthreaded []string `json:"unthreaded,omitempty"`

	// Fallback marks the engine-written record emitted when the agent phase
	// succeeded but wrote no output file.
	Fallback bool `json:"fallback,omitempty"`

	// Error describes a failed attempt.
	Error *ResultError `json:"error,omitempty"`

	// Timing holds the sequencer's per-phase clocks (nanoseconds).
	Timing map[string]time.Duration `json:"timing,omitempty"`

	// Attempt echoes FABER_ATTEMPT.
	Attempt int `json:"attempt"`
}

// ResultError is the error half of a failed attempt record.
type ResultError struct {
	// Reason is one of the Reason* constants.
	Reason string `json:"reason"`

	// Detail is a human-readable, secret-free elaboration (collected schema
	// violations, the failing phase's message).
	Detail string `json:"detail,omitempty"`

	// Handoff is the fail-stop record's file name relative to the result
	// directory (HandoffFile), when one was written.
	Handoff string `json:"handoff,omitempty"`
}

// Handoff is the structured fail-stop record written beside the attempt
// record: everything the interactive recovery mode needs to reconstruct the
// box. It never carries secret values — Inputs holds the bound input values
// only. Keying names the Inputs key vocabulary: HandoffKeyingSlot means
// declared slot names (re-entry consumes them without translation); absent
// means a pre-versioning record keyed by FABER_INPUT_* env tokens, which
// re-entry translates through the template's declared slots.
type Handoff struct {
	Keying     string            `json:"keying,omitempty"`
	Phase      string            `json:"phase"`
	Reason     string            `json:"reason"`
	ExitCode   int               `json:"exit_code,omitempty"`
	StderrTail string            `json:"stderr_tail,omitempty"`
	Inputs     map[string]string `json:"inputs"`
	Workdir    string            `json:"workdir"`
}

// WriteResultFile writes the attempt record atomically (temp file plus rename
// within the result directory) so the mounted directory never exposes a
// half-written record. It stamps the writer's ContractVersion so the host
// can assert the record's vintage on extract.
func WriteResultFile(dir string, rec Result) error {
	if rec.Contract == 0 {
		rec.Contract = ContractVersion
	}
	return writeJSON(dir, ResultFile, rec)
}

// MaxRecordBytes bounds every host-side read of a container-written record
// file (result.json, handoff.json). It matches the journal's line bound: a
// record the journal could not replay must fail at the boundary, and a
// hostile box must not be able to balloon host memory through a mounted file.
const MaxRecordBytes = 64 << 20

// ReadBoundedFile reads a container-written file with the record size bound.
// Oversize is an error, never a truncation — a partially read record must not
// be interpreted.
func ReadBoundedFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, MaxRecordBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxRecordBytes {
		return nil, fmt.Errorf("contract: %s exceeds the %d-byte record bound", filepath.Base(path), MaxRecordBytes)
	}
	return raw, nil
}

// ReadResultFile parses the attempt record from the result directory. The
// read is size-bounded: the box is untrusted and the record crosses the
// container boundary.
func ReadResultFile(dir string) (Result, error) {
	raw, err := ReadBoundedFile(filepath.Join(dir, ResultFile))
	if err != nil {
		return Result{}, fmt.Errorf("contract: read %s: %w", ResultFile, err)
	}
	var rec Result
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Result{}, fmt.Errorf("contract: parse %s: %w", ResultFile, err)
	}
	if rec.Status != StatusOK && rec.Status != StatusFailed {
		return Result{}, fmt.Errorf("contract: %s: unknown status %q", ResultFile, rec.Status)
	}
	return rec, nil
}

// WriteHandoffFile writes the fail-stop record atomically.
func WriteHandoffFile(dir string, h Handoff) error {
	return writeJSON(dir, HandoffFile, h)
}

// writeJSON marshals v and renames it into place under dir/name.
func writeJSON(dir, name string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("contract: marshal %s: %w", name, err)
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return fmt.Errorf("contract: write %s: %w", name, err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("contract: write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("contract: write %s: %w", name, err)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("contract: write %s: %w", name, err)
	}
	return nil
}
