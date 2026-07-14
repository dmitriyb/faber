package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/dmitriyb/faber/agent"
	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
	"github.com/dmitriyb/faber/security"
)

// InteractiveRunner launches one reconstructed box attached to the operator's
// terminal. The wiring supplies the real implementation (a docker run with a
// TTY over the assembled argv); tests fake it. It is the one pipeline seam
// that inherently needs a terminal and a real container runtime.
type InteractiveRunner interface {
	RunInteractive(ctx context.Context, spec infra.RunSpec) error
}

// containerHandoffDir is where the failed attempt's preserved state is
// surfaced read-only inside the reconstructed box.
const containerHandoffDir = "/faber/handoff"

// Reentry implements the failure module's BoxReentry seam: it reconstructs a
// failed step's box — same image tag, same security bindings, same resolved
// inputs exported as the step env — with the entry program replaced by an
// interactive shell and the failed attempt's handoff state mounted read-only.
// Nothing is journaled; the session is observation, not execution.
type Reentry struct {
	IR          *config.IR // the run's IR, for the failed node's template
	Images      ImageTagger
	Bindings    BindingPreparer
	Interactive InteractiveRunner
	EntryBinary string
	Shell       []string // in-container entry; default ["/bin/sh"]

	// Security configuration, resolved by the wiring like AgentBoxes'. Note the
	// absence of Services: re-entry is wired with a credential-free binding set
	// (security.NewBindingSetWithoutCredentials), so no service declaration is
	// ever consulted here — see Reenter.
	Network    *config.NetworkDef
	Remote     *config.RemoteDef
	Identities map[string]config.IdentityDef
}

var _ failure.BoxReentry = (*Reentry)(nil)

// Reenter implements failure.BoxReentry.
func (r *Reentry) Reenter(ctx context.Context, t failure.InteractiveTarget) error {
	if r.Interactive == nil {
		return fmt.Errorf("pipeline: interactive re-entry: no interactive runner is wired")
	}
	node := findIRNode(r.IR, t.StepID)
	if node == nil {
		return fmt.Errorf("pipeline: interactive re-entry: step %s is not in the run's IR (generate-instance steps cannot be reconstructed in this pass)", t.StepID)
	}
	if node.Kind != config.KindAgent || node.Template == nil {
		return fmt.Errorf("pipeline: interactive re-entry: step %s runs no box (kind %s)", t.StepID, node.Kind)
	}
	handoffPath, ok := t.HandoffPath()
	if !ok {
		return fmt.Errorf("pipeline: interactive re-entry: step %s preserved no handoff state to reconstruct from", t.StepID)
	}
	raw, err := os.ReadFile(handoffPath)
	if err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: read handoff record: %w", err)
	}
	var handoff contract.Handoff
	if err := json.Unmarshal(raw, &handoff); err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: parse handoff record: %w", err)
	}
	tag := ""
	if r.Images != nil {
		if tag, err = r.Images.Tag(node.Template); err != nil {
			return fmt.Errorf("pipeline: interactive re-entry: resolve image tag: %w", err)
		}
	}
	attempt := t.Record.Result.Attempt
	if attempt < 1 {
		attempt = 1
	}
	sessionDir := filepath.Join(t.RunDir, "interactive", pathToken(t.StepID))
	resultDir := filepath.Join(sessionDir, "result")
	scratchDir := filepath.Join(sessionDir, "scratch")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: %w", err)
	}
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: %w", err)
	}

	spec, err := agent.BuildRunSpec(agent.BoxSpec{
		RunID:       t.Header.RunID + "-interactive",
		NodeID:      t.StepID,
		Attempt:     attempt,
		Template:    node.Template,
		Image:       tag,
		Inputs:      handoff.Inputs,
		ResultDir:   resultDir,
		EntryBinary: r.EntryBinary,
		ContextHook: node.Template.Hooks.Context,
		PreludeHook: node.Template.Hooks.Prelude,
		SkillsDir:   skillsDir(node.Template),
		SkillsLink:  skillsLink(node.Template),
	})
	if err != nil {
		return err
	}

	var identity *config.IdentityDef
	if node.Template.Identity != "" {
		if def, ok := r.Identities[node.Template.Identity]; ok {
			identity = &def
		}
	}
	// The re-entry debug shell carries no credentials. r.Bindings is the
	// credential-free binding set: the shell observes the failed step, it never
	// runs the agent, and it cannot materialize the stdin secrets payload (the
	// raw shell replaces the box sequencer), so no token is resolved and none is
	// streamed. Deliberately no Services here — the credential broker is not
	// composed for re-entry; an operator who needs a secret sets it by hand.
	asm, err := r.Bindings.Prepare(ctx, security.StepSpec{
		NodeID:     t.StepID,
		Network:    r.Network,
		Remote:     r.Remote,
		Identity:   identity,
		Runtime:    node.Template.Runtime,
		Repo:       handoff.Inputs["repo"],
		ScratchDir: scratchDir,
	})
	if err != nil {
		return err
	}
	defer func() { _ = asm.Teardown(ctx) }()
	spec.Bindings = asm.Args

	// The operator's shell replaces the phase sequencer; the failed attempt's
	// preserved state rides along read-only.
	shell := r.Shell
	if len(shell) == 0 {
		shell = []string{"/bin/sh"}
	}
	spec.Entry = shell
	spec.Name = spec.Name + "-i" + strconv.Itoa(attempt)
	spec.Mounts = append(spec.Mounts, infra.Mount{
		Host:      filepath.Dir(handoffPath),
		Container: containerHandoffDir,
		ReadOnly:  true,
	})
	return r.Interactive.RunInteractive(ctx, spec)
}

// findIRNode looks a node id up across the IR, recursing into inlined
// sub-workflow graphs.
func findIRNode(ir *config.IR, id string) *config.Node {
	if ir == nil {
		return nil
	}
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		if n.ID == id {
			return n
		}
		if n.Sub != nil {
			if found := findIRNode(n.Sub, id); found != nil {
				return found
			}
		}
	}
	return nil
}
