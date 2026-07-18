// Package agent is the host half of the box: it assembles the box run
// contract (the FABER_* environment and engine mounts the infra runner
// needs) for one agent step attempt, and reads the typed attempt record back
// across the container boundary with defensive re-validation.
//
// The in-container half — the fixed phase order context -> prelude -> agent
// -> result — lives in the agent/box package and ships as the faber-box
// binary; the shared contract (env names, record shapes, output-schema
// validation) lives in agent/contract. The pipeline executor composes this
// package with the security module's binding set and the infra module's
// container runner, and adapts the Result record to the failure module's
// step-result type at integration.
package agent

import (
	"github.com/dmitriyb/faber/agent/contract"
)

// Aliases for the contract types the pipeline consumes, so ordinary
// integrations import only this package.
type (
	// Result is one step attempt's typed record (result.json).
	Result = contract.Result

	// ResultError is the error half of a failed record.
	ResultError = contract.ResultError

	// Handoff is the structured fail-stop record beside a failed attempt.
	Handoff = contract.Handoff

	// OutputSchema is the template's declared output fields.
	OutputSchema = contract.OutputSchema
)

// Statuses of a Result record.
const (
	StatusOK     = contract.StatusOK
	StatusFailed = contract.StatusFailed
)
