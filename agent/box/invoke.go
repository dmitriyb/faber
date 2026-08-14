package box

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
)

// Invocation assembles the one headless agent invocation of the box — the
// only nondeterministic phase, and atomic: there is no resuming into an
// agent's chain of thought, only re-running the whole step. The CLI name and
// skill are opaque user config, and the invocation's shape is the concrete
// profile the host compiled (or the built-in default): faber hardcodes no
// agent vendor, and no vendor literal appears in this file.
type Invocation struct {
	CLI       string                // agent binary from the template's package set
	Profile   config.ResolvedInvoke // concrete dialect; every field final
	Skill     string
	Body      string // CONTEXT.md bytes, verbatim — the hooks authored it
	Extra     string // optional operator note for a single run
	Model     string // pass-through; empty omits the pair (mandatory template config in engine runs)
	Effort    string // pass-through; empty omits the pair (mandatory template config in engine runs)
	MaxBudget string
}

// Prompt expands the profile's prompt template over the closed placeholder
// set: {skill}, {body} (the bundle body verbatim), and {extra} (the clearly
// delimited optional trailer, or empty). strings.Replacer substitutes in one
// pass over the template only — substituted text is never re-scanned, so
// bundle bytes can never inject into the template.
func (i Invocation) Prompt() string {
	extra := ""
	if i.Extra != "" {
		extra = "\n\nADDITIONAL INSTRUCTION: " + i.Extra
	}
	return strings.NewReplacer(
		"{skill}", i.Skill,
		"{body}", i.Body,
		"{extra}", extra,
	).Replace(i.Profile.PromptTemplate)
}

// Argv is the profile-expanded headless invocation:
// CLI + subcommand + prompt (flagged or positional) + the skill's flag pair
// (flag mode only) + the fixed tail + the model/effort/budget pairs. A
// pass-through pair is emitted only when BOTH its profile flag and its value
// are set — an empty value is today's omission path, an empty flag a harness
// without that knob. Under the default profile the result is the full
// permission bypass: the sealed environment is the restriction, and a second
// in-container permission gate would be a control enforced by the untrusted
// thing it is meant to control.
func (i Invocation) Argv() []string {
	p := i.Profile
	argv := append([]string{i.CLI}, p.Subcommand...)
	if p.PromptFlag != "" {
		argv = append(argv, p.PromptFlag)
	}
	argv = append(argv, i.Prompt())
	if p.SkillMode == config.SkillModeFlag {
		argv = append(argv, p.SkillFlag, i.Skill)
	}
	argv = append(argv, p.FixedFlags...)
	pair := func(flag, val string) {
		if flag != "" && val != "" {
			argv = append(argv, flag, val)
		}
	}
	pair(p.ModelFlag, i.Model)
	pair(p.EffortFlag, i.Effort)
	pair(p.BudgetFlag, i.MaxBudget)
	return argv
}

// runAgent is phase 9. The child environment is the box environment plus the
// bundle's sidecar values, so anything the prelude derived is visible to the
// skill. stdout and stderr stream to the container log and are never parsed:
// the result file is the only machine-readable channel out of this phase.
// When the prelude's skip request was honored (an agent-skippable template),
// the phase is a logged no-op: no process, no prompt, no model call — the
// postlude and result phases run unchanged, and the result phase enforces
// the output contract exactly as if a quiet agent had run.
func (b *Box) runAgent(ctx context.Context) error {
	if b.skipAgent {
		b.Log.InfoContext(ctx, "agent skipped by prelude", "skill", b.Env.Skill)
		return nil
	}
	inv := Invocation{
		CLI:       b.Env.AgentCLI,
		Profile:   b.Env.Invoke,
		Skill:     b.Env.Skill,
		Body:      b.Bundle.Doc,
		Extra:     b.Env.ExtraInstruction,
		Model:     b.Env.Model,
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
