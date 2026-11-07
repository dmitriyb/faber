package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
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

// OutputLogName is the per-attempt file under the attempt directory that
// receives the container's combined stdout+stderr as it streams.
const OutputLogName = "output.log"

// BoxAttempt is one attempt of one agent node at the executor's box-run seam.
// Attempt numbering is continuous across defer re-queues.
type BoxAttempt struct {
	RunID    string
	RunDir   string // the run's directory; result dirs live beneath it
	NodeID   string
	Attempt  int
	Template *config.ResolvedTemplate
	Image    string
	Inputs   map[string]any // resolved input slot values

	// Sessions is the per-run transcript-capture toggle (--sessions, ORed
	// with the journal header on resume). Capture happens only when it is on
	// AND the template's resolved profile names a session_dir.
	Sessions bool
}

// BoxResult is one finished attempt: the record adapted to the failure
// module's shape plus the advisory usage sidecar the box deposited (nil if
// none).
type BoxResult struct {
	Result failure.Result
	Usage  map[string]int64
}

// BoxRunner is the box-run boundary — the one seam unit tests fake. A non-nil
// error means the attempt never launched (the failure policy synthesizes a
// launch-failure record); everything that ran, however badly, is a Result.
type BoxRunner interface {
	RunAttempt(ctx context.Context, box BoxAttempt) (BoxResult, error)
}

// ImageTagger resolves a template's immutable image tag (the infra image
// builder's deterministic tag at integration; a fixed fake in tests). The tag
// participates in the journal's input hash, so a rebuilt toolset re-runs.
type ImageTagger interface {
	Tag(template *config.ResolvedTemplate) (string, error)
}

// ContainerRunner is the slice of infra's container-run primitive the box
// composition needs. *infra.ContainerRunner satisfies it.
type ContainerRunner interface {
	Run(ctx context.Context, spec infra.ContainerSpec) (infra.ContainerResult, error)
}

// BindingPreparer is the slice of the security module's binding assembly the
// box composition needs. *security.BindingSet satisfies it.
type BindingPreparer interface {
	Prepare(ctx context.Context, step security.StepSpec) (security.Assembled, error)
}

// AgentBoxes is the production BoxRunner: agent.BuildContainerSpec assembles the
// engine half of the run contract, the security bindings are spliced into the
// spec verbatim, the container runs, bindings tear down, and
// agent.ExtractResult re-validates the record host-side. The container exit
// code is never authoritative — a missing or garbled record synthesizes a
// box-vanished failure. Construction happens at cmd wiring; unit tests fake
// BoxRunner instead.
type AgentBoxes struct {
	Containers ContainerRunner
	Bindings   BindingPreparer

	// Security configuration, resolved from the orchestrator config by the
	// wiring (the executor itself never reads Config).
	Network    *config.NetworkDef
	Remote     *config.RemoteDef
	Identities map[string]config.IdentityDef
	Services   map[string]config.ServiceDef

	// GitIdentities maps a template's identity role to the committer identity
	// its boxes use, sourced from the role registry by the wiring — a role
	// property in an explicit per-host file, never process environment. A role
	// absent from the map (or with an empty email) simply contributes nothing;
	// gated boxes then fail their signing phase, which is the designed
	// fail-fast for an unregistered committer identity.
	GitIdentities map[string]GitIdentity

	// Check marks a check run: every box gets FABER_CHECK=1 (no agent,
	// placeholder outputs). The sandbox itself is the Bindings the wiring
	// composed; this field only tells the box.
	Check bool

	Log *slog.Logger
}

// GitIdentity is one role's committer identity as the boxes will write it
// into commit objects.
type GitIdentity struct {
	Name  string
	Email string
}

// RunAttempt implements BoxRunner.
func (b *AgentBoxes) RunAttempt(ctx context.Context, box BoxAttempt) (BoxResult, error) {
	if box.Template == nil {
		return BoxResult{}, fmt.Errorf("pipeline: box %s: no resolved template", box.NodeID)
	}
	log := b.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	attemptDir := AttemptDir(box.RunDir, box.NodeID, box.Attempt)
	resultDir := ResultDir(box.RunDir, box.NodeID, box.Attempt)
	scratchDir := filepath.Join(attemptDir, "scratch")
	// A reused attempt dir (a resumed run's attempt numbering restarts; a
	// crashed launch may have deposited files) can hold a stale result.json
	// that ExtractResult would adopt as this attempt's outcome. Clear it so
	// whatever the extractor reads was written by this attempt's container —
	// but preserve any captured session transcript first: it is the durable
	// record of the failed execution the resume is retrying, and the routine
	// fail→resume flow must not silently destroy the feature's core artifact.
	if err := preserveSessions(attemptDir); err != nil {
		return BoxResult{}, fmt.Errorf("pipeline: box %s: preserve prior sessions: %w", box.NodeID, err)
	}
	if err := os.RemoveAll(attemptDir); err != nil {
		return BoxResult{}, fmt.Errorf("pipeline: box %s: clear stale attempt dir: %w", box.NodeID, err)
	}
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return BoxResult{}, fmt.Errorf("pipeline: box %s: %w", box.NodeID, err)
	}
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return BoxResult{}, fmt.Errorf("pipeline: box %s: %w", box.NodeID, err)
	}

	inputs, err := stringifyInputs(box.Inputs)
	if err != nil {
		return BoxResult{}, fmt.Errorf("pipeline: box %s: %w", box.NodeID, err)
	}
	// The live sessions bind: a fresh empty per-attempt dir (the scrub above
	// cleared only THIS attempt's dir, so earlier attempts' transcripts
	// persist), host-owned by the run user like the result dir, mounted by
	// the run-spec assembler at $HOME/<profile session_dir>. Both gates —
	// the per-run toggle and the profile's session_dir — must be on.
	sessionsDir := ""
	if box.Sessions && box.Template.Invoke != nil && box.Template.Invoke.SessionDir != "" {
		sessionsDir = filepath.Join(attemptDir, "sessions")
		if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
			return BoxResult{}, fmt.Errorf("pipeline: box %s: %w", box.NodeID, err)
		}
	}
	spec, err := agent.BuildContainerSpec(agent.BoxSpec{
		RunID:       box.RunID,
		NodeID:      box.NodeID,
		Attempt:     box.Attempt,
		Template:    box.Template,
		Image:       box.Image,
		Inputs:      inputs,
		ResultDir:   resultDir,
		SkillsLink:  skillsLink(box.Template),
		Model:       box.Template.Model,
		Effort:      box.Template.Effort,
		GitName:     b.GitIdentities[box.Template.Identity].Name,
		GitEmail:    b.GitIdentities[box.Template.Identity].Email,
		SessionsDir: sessionsDir,
		Check:       b.Check,
	})
	if err != nil {
		return BoxResult{}, err
	}

	var identity *config.IdentityDef
	if box.Template.Identity != "" {
		if def, ok := b.Identities[box.Template.Identity]; ok {
			identity = &def
		}
	}
	asm, err := b.Bindings.Prepare(ctx, security.StepSpec{
		NodeID:       box.NodeID,
		Network:      b.Network,
		Remote:       b.Remote,
		Identity:     identity,
		IdentityRole: box.Template.Identity,
		Services:     b.Services,
		Runtime:      box.Template.Runtime,
		Repo:         inputs["repo"],
		ScratchDir:   scratchDir,
	})
	if err != nil {
		return BoxResult{}, err
	}
	spec.Bindings = asm.Args
	// The attempt's combined output streams into a durable file beside its
	// result dir — the only record of hook and phase output that outlives the
	// bounded in-memory tail. The attempt dir was scrubbed above, so the file
	// is this attempt's alone.
	outFile, err := os.OpenFile(filepath.Join(attemptDir, OutputLogName), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return BoxResult{}, fmt.Errorf("pipeline: box %s: open output log: %w", box.NodeID, err)
	}
	defer outFile.Close()
	spec.Output = outFile
	// The credentials pairing is atomic: a non-empty file-mode secrets payload
	// is copied into ContainerSpec.StdinSecrets and the box's stdin signal is set in
	// the same step, never one without the other (see spec/infra/impl_run_argv.md
	// and spec/pipeline/impl_scheduling.md). This is the only host-side owner of
	// ContainerSpec.Env assembly at the step-runner seam.
	if len(asm.SecretsStdin) > 0 {
		spec.StdinSecrets = asm.SecretsStdin
		spec.Env[contract.EnvSecretsStdin] = "1"
	}

	runRes, runErr := b.Containers.Run(ctx, spec)
	if terr := asm.Teardown(ctx); terr != nil {
		log.WarnContext(ctx, "binding teardown", "step", box.NodeID, "err", terr)
	}
	if runErr != nil && ctx.Err() != nil {
		return BoxResult{}, runErr
	}
	// The exit code is data, never authoritative: the mounted record decides.
	rec, err := agent.ExtractResult(resultDir, box.Template.Output)
	if err != nil {
		return BoxResult{}, err
	}
	if runErr != nil && rec.Status == agent.StatusFailed && rec.Error != nil && rec.Error.Reason == contract.ReasonBoxVanished {
		// The container never produced a record because the actuation itself
		// failed (daemon unreachable, bad flag, missing image). The true cause
		// must live in the failure record, not at debug level — otherwise the
		// operator sees only a synthetic "box vanished" while each retry burns
		// against the same dead daemon.
		rec.Error.Detail = fmt.Sprintf("container actuation failed: %v (%s; output tail: %s)",
			runErr, rec.Error.Detail, outputTail(runRes.Output, 2048))
	}
	res := adaptResult(rec, box, runRes, log)
	return BoxResult{Result: res, Usage: readUsage(resultDir, log)}, nil
}

// outputTail renders the last max bytes of captured container output for a
// failure record's detail.
func outputTail(out []byte, max int) string {
	if len(out) > max {
		out = out[len(out)-max:]
	}
	return string(out)
}

// adaptResult maps the agent module's attempt record onto the failure
// module's result shape. Only ok payloads thread; a failed record's handoff
// pointer is re-rooted under the run directory so the interactive mode can
// resolve it — and, because the pointer is box-authored bytes, it must
// resolve strictly under the attempt's result dir (the same discipline the
// image builder applies to skill names): a traversal like "../../.." would otherwise
// bind an arbitrary host directory into the operator's re-entry container.
func adaptResult(rec agent.Result, box BoxAttempt, runRes infra.ContainerResult, log *slog.Logger) failure.Result {
	out := failure.Result{
		Attempt:      box.Attempt,
		AgentSkipped: rec.AgentSkipped,
		Check:        rec.Check,
		Timing: failure.Timing{
			Started:  runRes.Started,
			Finished: runRes.Started.Add(runRes.Duration),
		},
	}
	if rec.Status == agent.StatusOK {
		raw, err := json.Marshal(rec.Payload)
		if err != nil {
			return failure.Result{
				Status:       failure.StatusFailed,
				Error:        &failure.ErrorRecord{Reason: contract.ReasonOutputSchema, Detail: fmt.Sprintf("encode payload: %v", err)},
				AgentSkipped: rec.AgentSkipped,
				Check:        rec.Check,
				Attempt:      box.Attempt,
				Timing:       out.Timing,
			}
		}
		out.Status = failure.StatusOK
		out.Payload = raw
		return out
	}
	if rec.Status == agent.StatusHalted {
		// ExtractResult already guaranteed a halt arm with a reason and
		// dropped the payload. The halt fields are the halter's vocabulary,
		// carried verbatim (the report sanitizes at render).
		out.Status = failure.StatusHalted
		out.Halt = &failure.HaltRecord{Reason: rec.Halt.Reason, Detail: rec.Halt.Detail, Phase: rec.Halt.Phase}
		return out
	}
	out.Status = failure.StatusFailed
	errRec := &failure.ErrorRecord{Reason: contract.ReasonBoxVanished, Detail: "failed record without an error body"}
	if rec.Error != nil {
		reason, detail := sanitizeBoxReason(rec.Error.Reason), rec.Error.Detail
		if reason == "" {
			// The box reported failure with an error body but named no reason
			// (e.g. `"error":{}`). An empty reason fails Result.Validate, so
			// synthesize the same fallback the missing-error-body case uses,
			// keeping any detail the box did provide.
			reason = contract.ReasonBoxVanished
			if detail == "" {
				detail = "box reported failure without a reason"
			}
		}
		errRec = &failure.ErrorRecord{Reason: reason, Detail: detail}
		if rec.Error.Handoff != "" {
			resultRel := filepath.Join("boxes", pathToken(box.NodeID), "attempt-"+strconv.Itoa(box.Attempt), "result")
			joined := filepath.Join(resultRel, rec.Error.Handoff)
			if !pathWithin(joined, resultRel) {
				log.Warn("box-authored handoff pointer escapes the attempt result dir; dropping it",
					"step", box.NodeID, "handoff", rec.Error.Handoff)
				errRec.Detail += " (handoff pointer escaped the attempt result dir and was dropped)"
			} else {
				errRec.Handoff = joined
			}
		}
	}
	out.Error = errRec
	return out
}

// pathWithin reports whether the cleaned relative path p lies strictly under
// base (itself clean, relative, separator-normalized).
func pathWithin(p, base string) bool {
	return strings.HasPrefix(p, base+string(filepath.Separator))
}

// sanitizeBoxReason namespaces box-authored failure reasons that collide with
// the pipeline's reserved journal vocabulary (the skip encodings and the
// annotation markers). The journal decoders already refuse to trust these
// reasons on executed records — this is defense in depth so a hostile box's
// collision is visible as exactly what it is.
func sanitizeBoxReason(reason string) string {
	switch reason {
	case reasonSkippedCondition, reasonSkippedDependency, reasonSkippedHalt, reasonDeferred, reasonCached:
		return "box:" + reason
	}
	return reason
}

// maxUsageBytes bounds the advisory usage sidecar read — the box writes a
// small flat object; anything larger is not a usage report.
const maxUsageBytes = 1 << 20

// readUsage decodes the advisory usage sidecar. Absence is simply no usage;
// malformation and oversize are logged (a silently dropped sidecar reads as
// "the vendor reported nothing" and quietly disables the reported tier); and
// values are clamped non-negative — the sidecar is box-authored bytes, and a
// negative count must never reach the budget ledger as a refund.
func readUsage(resultDir string, log *slog.Logger) map[string]int64 {
	f, err := os.Open(filepath.Join(resultDir, contract.UsageFile))
	if err != nil {
		return nil // absent: the box deposited no usage
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxUsageBytes+1))
	if err != nil || len(raw) > maxUsageBytes {
		log.Warn("usage sidecar unreadable or over the size bound; ignoring it", "err", err, "bytes", len(raw))
		return nil
	}
	var usage map[string]int64
	if err := json.Unmarshal(raw, &usage); err != nil {
		log.Warn("usage sidecar undecodable; ignoring it (metering's reported tier will see no usage)", "err", err)
		return nil
	}
	for k, v := range usage {
		if v < 0 {
			log.Warn("negative usage value clamped to zero", "field", k, "value", v)
			usage[k] = 0
		}
	}
	return usage
}

// preserveSessions moves a reused attempt dir's non-empty captured-session
// directory aside — to the sibling `<attemptDir>.sessions.<k>`, first free k —
// before the pre-attempt scrub deletes the dir. A resumed run restarts
// attempt numbering, so without this the re-run of a failed step would erase
// the very transcript that explains the failure. Preservation is
// unconditional on the current toggle: whatever a prior execution captured is
// a record, whether or not this one captures. An absent or empty sessions dir
// moves nothing.
func preserveSessions(attemptDir string) error {
	src := filepath.Join(attemptDir, "sessions")
	entries, err := os.ReadDir(src)
	if err != nil || len(entries) == 0 {
		return nil // absent or empty: nothing worth preserving
	}
	for k := 1; ; k++ {
		dst := fmt.Sprintf("%s.sessions.%d", attemptDir, k)
		if _, err := os.Stat(dst); err == nil {
			continue // taken by an earlier preservation
		} else if !os.IsNotExist(err) {
			return err
		}
		return os.Rename(src, dst)
	}
}

// stringifyInputs renders resolved input values for the box env contract.
func stringifyInputs(inputs map[string]any) (map[string]string, error) {
	out := make(map[string]string, len(inputs))
	for _, slot := range sortedKeys(inputs) {
		s, err := scalarString(inputs[slot])
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", slot, err)
		}
		out[slot] = s
	}
	return out, nil
}

// skillsLink reads the template's optional skills-discovery path, returning ""
// when the template declares no skills leg (absence = current behavior).
func skillsLink(t *config.ResolvedTemplate) string {
	if t.Skills == nil {
		return ""
	}
	return t.Skills.Link
}

// AttemptDir is the host directory of one attempt of one node under a run
// directory — where result/, output.log, and sessions/ live. Exported so a
// reader (the TUI) finds them without re-deriving the path token.
func AttemptDir(runDir, nodeID string, attempt int) string {
	return filepath.Join(runDir, "boxes", pathToken(nodeID), "attempt-"+strconv.Itoa(attempt))
}

// ResultDir is the attempt's result directory: the box's mounted
// $FABER_RESULT_DIR on the host, where the records and the hooks' events
// file live.
func ResultDir(runDir, nodeID string, attempt int) string {
	return filepath.Join(AttemptDir(runDir, nodeID, attempt), "result")
}

// AttemptEvents reads the attempt's hooks events file, so a view finds it
// without re-deriving the path.
func AttemptEvents(runDir, nodeID string, attempt int) (contract.Events, error) {
	return contract.ReadEvents(filepath.Join(ResultDir(runDir, nodeID, attempt), contract.HooksEventsFile))
}

// pathToken maps a node id onto a filesystem-safe directory name. A short
// hash of the raw id keeps distinct ids distinct after character mapping.
func pathToken(id string) string {
	sum := sha256.Sum256([]byte(id))
	b := []byte(id)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
		default:
			b[i] = '_'
		}
	}
	return string(b) + "-" + hex.EncodeToString(sum[:4])
}
