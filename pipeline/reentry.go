package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dmitriyb/faber/agent"
	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
	"github.com/dmitriyb/faber/security"
)

// InteractiveSession is one reconstructed box for the operator: the spec of
// the held box, the frame stream it receives before the operator enters
// (the failed attempt's handoff as handoff/<rel>, a saved transcript as
// sessions/<rel>), and the exec that is the operator's entry.
type InteractiveSession struct {
	Spec    infra.ContainerSpec
	Input   io.Reader
	Session infra.Session
}

// InteractiveRunner enters one reconstructed box on the operator's terminal.
// The wiring supplies the real implementation (infra's hold, receive, attach,
// kill choreography); tests fake it. It is the one pipeline seam that
// inherently needs a terminal and a real container runtime.
type InteractiveRunner interface {
	RunInteractive(ctx context.Context, s InteractiveSession) error
}

// Reentry implements the failure module's BoxReentry seam: it reconstructs a
// failed step's box — same image tag, same security bindings, same resolved
// inputs exported as the step env — held on faber-box hold, with the failed
// attempt's handoff state (and a saved transcript) sent in over the frame
// channel rather than mounted, and the operator's shell or the harness's
// resumed session as a second exec. Nothing is journaled; the session is
// observation, not execution.
type Reentry struct {
	IR          *config.IR // the run's IR, for the failed node's template
	Images      ImageTagger
	Bindings    BindingPreparer
	Interactive InteractiveRunner
	Shell       []string // the operator's entry; default ["/bin/sh"]

	// Security configuration, resolved by the wiring like AgentBoxes'. Note the
	// absence of Services: re-entry is wired with a credential-free binding set
	// (security.NewBindingSetWithoutCredentials), so no service declaration is
	// ever consulted here — see Reenter.
	Network    *config.NetworkDef
	Remote     *config.RemoteDef
	Identities map[string]config.IdentityDef

	// Project prefixes the template's named volumes, as AgentBoxes'.
	Project string
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
	raw, err := contract.ReadBoundedFile(handoffPath)
	if err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: read handoff record: %w", err)
	}
	var handoff contract.Handoff
	if err := json.Unmarshal(raw, &handoff); err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: parse handoff record: %w", err)
	}
	inputs, err := handoffInputs(handoff, node.Template)
	if err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: %w", err)
	}
	// Prefer the tag the run was journaled against: re-entry reconstructs
	// the box the step actually ran, even when a pin or faber upgrade has
	// since moved the current tag derivation.
	tag := t.Header.Images[node.Template.Name]
	if tag == "" && r.Images != nil {
		if tag, err = r.Images.Tag(node.Template); err != nil {
			return fmt.Errorf("pipeline: interactive re-entry: resolve image tag: %w", err)
		}
	}
	attempt := t.Record.Result.Attempt
	if attempt < 1 {
		attempt = 1
	}
	// The scratch dir and container name carry a per-session salt: two
	// concurrent sessions on the same step must never share binding scratch
	// or collide on the container name.
	salt := sessionSalt()
	sessionDir := filepath.Join(t.RunDir, "interactive", pathToken(t.StepID)+"-"+salt)
	scratchDir := filepath.Join(sessionDir, "scratch")
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: %w", err)
	}
	// A session is observation, not execution: nothing durable may accumulate
	// under the salted dir, so it goes when the session exits.
	defer os.RemoveAll(sessionDir)

	spec, err := agent.BuildContainerSpec(agent.BoxSpec{
		RunID:      t.Header.RunID + "-interactive",
		NodeID:     t.StepID,
		Attempt:    attempt,
		Template:   node.Template,
		Image:      tag,
		Inputs:     inputs,
		SkillsLink: skillsLink(node.Template),
		Model:      node.Template.Model,
		Effort:     node.Template.Effort,
		Project:    r.Project,
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
		NodeID:       t.StepID,
		Network:      r.Network,
		Remote:       r.Remote,
		Identity:     identity,
		IdentityRole: node.Template.Identity,
		Runtime:      node.Template.Runtime,
		Repo:         inputs["repo"],
		ScratchDir:   scratchDir,
		Image:        tag,
	})
	if err != nil {
		return err
	}
	defer func() { _ = asm.Teardown(ctx) }()
	spec.Bindings = asm.Args
	spec.Attach = asm.Attach

	// The operator's entry is the harness's own resumed session — the
	// profile's resume_argv over the saved transcript — whenever the failed
	// attempt saved one and the operator did not force --shell; the
	// transcript is sent into the box (never bound), so the host record stays
	// immutable while the ephemeral session diverges. Fallback: the shell.
	entry := r.Shell
	if len(entry) == 0 {
		entry = []string{"/bin/sh"}
	}
	savedSessions := ""
	if inv := node.Template.Invoke; !t.Shell && inv != nil && len(inv.ResumeArgv) > 0 && inv.SessionDir != "" {
		target := path.Join(contract.ContainerHome, inv.SessionDir)
		if !strings.HasPrefix(target, contract.ContainerHome+"/") {
			return fmt.Errorf("pipeline: interactive re-entry: profile session_dir %q does not resolve under %s", inv.SessionDir, contract.ContainerHome)
		}
		saved := filepath.Join(t.RunDir, "boxes", pathToken(t.StepID), "attempt-"+strconv.Itoa(attempt), "sessions")
		if hasEntries(saved) {
			savedSessions = saved
			spec.Env[contract.EnvSessionsDir] = target
			entry = inv.ResumeArgv
		}
	}
	spec.Name = spec.Name + "-i" + strconv.Itoa(attempt) + "-" + salt
	// The box is held, not sequenced: the operator's entry is a second exec.
	spec.Entry = []string{contract.ContainerEntry, "hold"}
	// The handoff lands on a tmpfs of the held box; faber-box receive fills
	// it from the stream and chowns it to the run user.
	spec.Mounts = append(spec.Mounts, infra.Mount{Kind: infra.KindTmpfs, Container: contract.ContainerHandoffDir})

	input, produced := handoffStream(filepath.Dir(handoffPath), savedSessions)
	runErr := r.Interactive.RunInteractive(ctx, InteractiveSession{
		Spec:  spec,
		Input: input,
		Session: infra.Session{
			Entry: entry,
			// The exec, not the sequencer, is the operator's entry, so no
			// preamble exports HOME; pin it to the box home or the harness
			// would look for its session under the image default.
			Env:     map[string]string{"HOME": contract.ContainerHome},
			User:    spec.Env[contract.EnvRunUID] + ":" + spec.Env[contract.EnvRunGID],
			Workdir: contract.ContainerWorkspace,
		},
	})
	input.Close()
	if perr := produced(); perr != nil {
		// The operator's view was incomplete whatever the session did; the
		// cause is host-side and must not be lost behind the exec's exit.
		return errors.Join(runErr, fmt.Errorf("pipeline: interactive re-entry: handoff stream: %w", perr))
	}
	return runErr
}

// handoffStream is the frame stream a re-entry receives: the handoff
// directory as handoff/<rel>, then the saved session (when one rides along)
// as sessions/<rel> with its mtimes and links, then end. It is produced on
// demand through a pipe, so a reader that never drains it costs nothing; a
// file that cannot be sent fails the stream rather than silently thinning
// the operator's view. produced waits for the producer and returns its
// error; the caller closes the reader first, so a producer the runner never
// drained ends too.
func handoffStream(handoffDir, savedSessions string) (input *io.PipeReader, produced func() error) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		w := contract.NewFrameWriter(pw)
		var skipped error
		skip := func(p string, err error) {
			if skipped == nil {
				skipped = fmt.Errorf("%s: %w", p, err)
			}
		}
		err := w.Tree(handoffDir, "handoff", "", false, skip)
		if err == nil && savedSessions != "" {
			err = w.Tree(savedSessions, "sessions", "", true, skip)
		}
		if err == nil {
			err = skipped
		}
		if err == nil {
			err = w.End()
		}
		pw.CloseWithError(err)
		if errors.Is(err, io.ErrClosedPipe) {
			err = nil // the runner stopped reading; not the stream's fault
		}
		done <- err
	}()
	return pr, func() error { return <-done }
}

// handoffInputs maps a handoff record's Inputs onto the slot-named run
// contract. A record marked HandoffKeyingSlot is already slot-keyed and used
// as is. A pre-versioning record is keyed by env tokens; the template's
// declared slots translate it forward (slot → token is total; the reverse is
// lossy, which is why the box now records slots). A record with no usable
// inputs while the template requires some is refused with a clear message
// rather than surfacing as per-slot contract violations.
func handoffInputs(h contract.Handoff, tpl *config.ResolvedTemplate) (map[string]string, error) {
	inputs := h.Inputs
	if h.Keying != contract.HandoffKeyingSlot {
		inputs = make(map[string]string, len(h.Inputs))
		for slot := range tpl.Inputs {
			if v, ok := h.Inputs[contract.SlotToken(slot)]; ok {
				inputs[slot] = v
			}
		}
	}
	for slot, def := range tpl.Inputs {
		if def.Required && inputs[slot] == "" {
			return nil, fmt.Errorf(
				"handoff record carries no value for required input %q (keying %q) — the record may predate this faber or was truncated; re-run the step instead",
				slot, h.Keying)
		}
	}
	return inputs, nil
}

// hasEntries reports whether dir exists and holds at least one entry — the
// "the failed attempt saved a session" gate; an absent or empty dir means
// capture was off (or the harness wrote nothing) and re-entry falls back to
// the shell.
func hasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// sessionSalt mints the short random token distinguishing concurrent
// interactive sessions on one step (dir suffix and container-name suffix).
func sessionSalt() string {
	var b [4]byte
	rand.Read(b[:]) // never fails per crypto/rand contract
	return hex.EncodeToString(b[:])
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
