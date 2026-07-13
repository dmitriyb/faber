package box

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/dmitriyb/faber/agent/contract"
)

// Invocation assembles the one headless agent invocation of the box — the
// only nondeterministic phase, and atomic: there is no resuming into an
// agent's chain of thought, only re-running the whole step. The CLI name and
// skill are opaque user config; faber hardcodes no agent vendor.
type Invocation struct {
	CLI       string // agent binary from the template's package set
	Skill     string
	Body      string // CONTEXT.md bytes, verbatim — the hooks authored it
	Extra     string // optional operator note for a single run
	Effort    string // pass-through; empty omits the flag
	MaxBudget string // pass-through; empty omits the flag
}

// Prompt is the three-part prompt: the skill-activating slash command, the
// bundle body verbatim, and the clearly delimited optional trailer.
func (i Invocation) Prompt() string {
	prompt := "/" + i.Skill + "\n\n" + i.Body
	if i.Extra != "" {
		prompt += "\n\nADDITIONAL INSTRUCTION: " + i.Extra
	}
	return prompt
}

// Argv is the headless invocation with full permission bypass: the sealed
// environment is the restriction, and a second in-container permission gate
// would be a control enforced by the untrusted thing it is meant to control.
func (i Invocation) Argv() []string {
	argv := []string{i.CLI, "-p", i.Prompt(), "--permission-mode", "bypassPermissions"}
	if i.Effort != "" {
		argv = append(argv, "--effort", i.Effort)
	}
	if i.MaxBudget != "" {
		argv = append(argv, "--max-budget-usd", i.MaxBudget)
	}
	return argv
}

// runAgent is phase 8. The child environment is the box environment plus the
// bundle's sidecar values, so anything the prelude derived is visible to the
// skill. stdout and stderr stream to the container log and are never parsed:
// the result file is the only machine-readable channel out of this phase.
func (b *Box) runAgent(ctx context.Context) error {
	inv := Invocation{
		CLI:       b.Env.AgentCLI,
		Skill:     b.Env.Skill,
		Body:      b.Bundle.Doc,
		Extra:     b.Env.ExtraInstruction,
		Effort:    b.Env.Effort,
		MaxBudget: b.Env.MaxBudget,
	}
	env := append([]string(nil), b.Environ...)
	for _, key := range slices.Sorted(maps.Keys(b.Bundle.Env)) {
		env = append(env, key+"="+b.Bundle.Env[key])
	}
	b.Log.InfoContext(ctx, "agent start", "cli", inv.CLI, "skill", inv.Skill)
	res, err := b.Runner.Stream(ctx, CmdSpec{Argv: inv.Argv(), Dir: b.Workdir, Env: env})
	if err != nil {
		return &boxError{Reason: contract.ReasonAgentFailed, Detail: fmt.Sprintf("run agent: %v", err)}
	}
	if res.ExitCode != 0 {
		// A budget-bound abort surfaces this way too; interpreting it is the
		// host-side meter's business, not the box's.
		return &boxError{
			Reason:     contract.ReasonAgentFailed,
			Detail:     fmt.Sprintf("agent exited %d", res.ExitCode),
			ExitCode:   res.ExitCode,
			StderrTail: string(res.StderrTail),
		}
	}
	return nil
}
