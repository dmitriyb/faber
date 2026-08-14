package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
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
	// The session dir and container name carry a per-session salt: two
	// concurrent sessions on the same step must never share mounts (the
	// second's setup would clear the first's live session dir) or collide on
	// the container name.
	salt := sessionSalt()
	sessionDir := filepath.Join(t.RunDir, "interactive", pathToken(t.StepID)+"-"+salt)
	resultDir := filepath.Join(sessionDir, "result")
	scratchDir := filepath.Join(sessionDir, "scratch")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: %w", err)
	}
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: %w", err)
	}
	// A session is observation, not execution: nothing durable may accumulate
	// under the salted dir, so it goes when the shell exits.
	defer os.RemoveAll(sessionDir)

	skillsHost, skillsCleanup, err := stageSkills(node.Template.Skills, sessionDir)
	if err != nil {
		return fmt.Errorf("pipeline: interactive re-entry: stage skills: %w", err)
	}
	defer skillsCleanup()
	spec, err := agent.BuildRunSpec(agent.BoxSpec{
		RunID:        t.Header.RunID + "-interactive",
		NodeID:       t.StepID,
		Attempt:      attempt,
		Template:     node.Template,
		Image:        tag,
		Inputs:       inputs,
		ResultDir:    resultDir,
		EntryBinary:  r.EntryBinary,
		ContextHook:  node.Template.Hooks.Context,
		PreludeHook:  node.Template.Hooks.Prelude,
		PostludeHook: node.Template.Hooks.Postlude,
		SkillsDir:    skillsHost,
		SkillsLink:   skillsLink(node.Template),
		Model:        node.Template.Model,
		Effort:       node.Template.Effort,
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
	})
	if err != nil {
		return err
	}
	defer func() { _ = asm.Teardown(ctx) }()
	spec.Bindings = asm.Args

	// The operator's entry replaces the phase sequencer; the failed attempt's
	// preserved state rides along read-only. The entry is the harness's own
	// resumed session — the profile's resume_argv over a COPY of the saved
	// transcript — whenever the failed attempt saved one and the operator did
	// not force --shell; the copy (never a bind of the archive) keeps the
	// host record immutable while the ephemeral session diverges, and it is
	// removed with the salted dir when the session exits. Fallback: the shell.
	entry := r.Shell
	if len(entry) == 0 {
		entry = []string{"/bin/sh"}
	}
	if inv := node.Template.Invoke; !t.Shell && inv != nil && len(inv.ResumeArgv) > 0 && inv.SessionDir != "" {
		target := path.Join(contract.ContainerHome, inv.SessionDir)
		if !strings.HasPrefix(target, contract.ContainerHome+"/") {
			return fmt.Errorf("pipeline: interactive re-entry: profile session_dir %q does not resolve under %s", inv.SessionDir, contract.ContainerHome)
		}
		saved := filepath.Join(t.RunDir, "boxes", pathToken(t.StepID), "attempt-"+strconv.Itoa(attempt), "sessions")
		if hasEntries(saved) {
			stateDir := filepath.Join(sessionDir, "harness-session")
			if err := copySessionTree(stateDir, saved); err != nil {
				return fmt.Errorf("pipeline: interactive re-entry: copy saved session: %w", err)
			}
			spec.Mounts = append(spec.Mounts, infra.Mount{Host: stateDir, Container: target})
			// The raw entry replaces the sequencer, so no preamble exports
			// HOME; pin it to the box home or the harness would look for its
			// session under the image default.
			spec.Env["HOME"] = contract.ContainerHome
			entry = inv.ResumeArgv
		}
	}
	spec.Entry = entry
	spec.Name = spec.Name + "-i" + strconv.Itoa(attempt) + "-" + salt
	spec.Mounts = append(spec.Mounts, infra.Mount{
		Host:      filepath.Dir(handoffPath),
		Container: containerHandoffDir,
		ReadOnly:  true,
	})
	return r.Interactive.RunInteractive(ctx, spec)
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

// copySessionTree mirrors a saved harness-session tree for re-entry. Unlike
// the skills stager's copyTree (world-readable real files, fresh mtimes,
// symlinks dropped — the right shape for a read-only engine mount), a session
// copy must stay faithful to what the harness wrote: harnesses locate "the
// most recent session" via file mtimes or a latest-pointer symlink, so a copy
// that flattens either resumes the wrong conversation. File modes and mtimes
// are preserved, symlinks are recreated verbatim (they resolve inside the
// operator's own debug container), and other non-regular entries are skipped.
func copySessionTree(dst, src string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := copyFile(target, p); err != nil {
			return err
		}
		if err := os.Chmod(target, info.Mode().Perm()); err != nil {
			return err
		}
		return os.Chtimes(target, info.ModTime(), info.ModTime())
	})
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
