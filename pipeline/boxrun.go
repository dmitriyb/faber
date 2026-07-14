package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
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
	Run(ctx context.Context, spec infra.RunSpec) (infra.RunResult, error)
}

// BindingPreparer is the slice of the security module's binding assembly the
// box composition needs. *security.BindingSet satisfies it.
type BindingPreparer interface {
	Prepare(ctx context.Context, step security.StepSpec) (security.Assembled, error)
}

// AgentBoxes is the production BoxRunner: agent.BuildRunSpec assembles the
// engine half of the run contract, the security bindings are spliced into the
// spec verbatim, the container runs, bindings tear down, and
// agent.ExtractResult re-validates the record host-side. The container exit
// code is never authoritative — a missing or garbled record synthesizes a
// box-vanished failure. Construction happens at cmd wiring; unit tests fake
// BoxRunner instead.
type AgentBoxes struct {
	Containers  ContainerRunner
	Bindings    BindingPreparer
	EntryBinary string // host path of the faber-box binary

	// Security configuration, resolved from the orchestrator config by the
	// wiring (the executor itself never reads Config).
	Network    *config.NetworkDef
	Remote     *config.RemoteDef
	Identities map[string]config.IdentityDef
	Services   map[string]config.ServiceDef

	GitName  string
	GitEmail string

	Log *slog.Logger
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
	attemptDir := filepath.Join(box.RunDir, "boxes", pathToken(box.NodeID), "attempt-"+strconv.Itoa(box.Attempt))
	resultDir := filepath.Join(attemptDir, "result")
	scratchDir := filepath.Join(attemptDir, "scratch")
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
	spec, err := agent.BuildRunSpec(agent.BoxSpec{
		RunID:       box.RunID,
		NodeID:      box.NodeID,
		Attempt:     box.Attempt,
		Template:    box.Template,
		Image:       box.Image,
		Inputs:      inputs,
		ResultDir:   resultDir,
		EntryBinary: b.EntryBinary,
		ContextHook: box.Template.Hooks.Context,
		PreludeHook: box.Template.Hooks.Prelude,
		SkillsDir:   skillsDir(box.Template),
		SkillsLink:  skillsLink(box.Template),
		GitName:     b.GitName,
		GitEmail:    b.GitEmail,
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
		NodeID:     box.NodeID,
		Network:    b.Network,
		Remote:     b.Remote,
		Identity:   identity,
		Services:   b.Services,
		Runtime:    box.Template.Runtime,
		Repo:       inputs["repo"],
		ScratchDir: scratchDir,
	})
	if err != nil {
		return BoxResult{}, err
	}
	spec.Bindings = asm.Args
	// The credentials pairing is atomic: a non-empty file-mode secrets payload
	// is copied into RunSpec.StdinSecrets and the box's stdin signal is set in
	// the same step, never one without the other (see spec/infra/impl_run_argv.md
	// and spec/pipeline/impl_scheduling.md). This is the only host-side owner of
	// RunSpec.Env assembly at the step-runner seam.
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
	res := adaptResult(rec, box, runRes)
	return BoxResult{Result: res, Usage: readUsage(resultDir)}, nil
}

// adaptResult maps the agent module's attempt record onto the failure
// module's result shape. Only ok payloads thread; a failed record's handoff
// pointer is re-rooted under the run directory so the interactive mode can
// resolve it.
func adaptResult(rec agent.Result, box BoxAttempt, runRes infra.RunResult) failure.Result {
	out := failure.Result{
		Attempt: box.Attempt,
		Timing: failure.Timing{
			Started:  runRes.Started,
			Finished: runRes.Started.Add(runRes.Duration),
		},
	}
	if rec.Status == agent.StatusOK {
		raw, err := json.Marshal(rec.Payload)
		if err != nil {
			return failure.Result{
				Status:  failure.StatusFailed,
				Error:   &failure.ErrorRecord{Reason: contract.ReasonOutputSchema, Detail: fmt.Sprintf("encode payload: %v", err)},
				Attempt: box.Attempt,
				Timing:  out.Timing,
			}
		}
		out.Status = failure.StatusOK
		out.Payload = raw
		return out
	}
	out.Status = failure.StatusFailed
	errRec := &failure.ErrorRecord{Reason: contract.ReasonBoxVanished, Detail: "failed record without an error body"}
	if rec.Error != nil {
		errRec = &failure.ErrorRecord{Reason: sanitizeBoxReason(rec.Error.Reason), Detail: rec.Error.Detail}
		if rec.Error.Handoff != "" {
			errRec.Handoff = filepath.Join("boxes", pathToken(box.NodeID),
				"attempt-"+strconv.Itoa(box.Attempt), "result", rec.Error.Handoff)
		}
	}
	out.Error = errRec
	return out
}

// sanitizeBoxReason namespaces box-authored failure reasons that collide with
// the pipeline's reserved journal vocabulary (the skip encodings and the
// annotation markers). The journal decoders already refuse to trust these
// reasons on executed records — this is defense in depth so a hostile box's
// collision is visible as exactly what it is.
func sanitizeBoxReason(reason string) string {
	switch reason {
	case reasonSkippedCondition, reasonSkippedDependency, reasonDeferred, reasonCached:
		return "box:" + reason
	}
	return reason
}

// readUsage decodes the advisory usage sidecar; absence or malformation is
// simply no usage.
func readUsage(resultDir string) map[string]int64 {
	raw, err := os.ReadFile(filepath.Join(resultDir, contract.UsageFile))
	if err != nil {
		return nil
	}
	var usage map[string]int64
	if err := json.Unmarshal(raw, &usage); err != nil {
		return nil
	}
	return usage
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

// skillsDir and skillsLink read the template's optional skills leg, returning
// "" when the template declares none (absence = current behavior).
func skillsDir(t *config.ResolvedTemplate) string {
	if t.Skills == nil {
		return ""
	}
	return t.Skills.Dir
}

func skillsLink(t *config.ResolvedTemplate) string {
	if t.Skills == nil {
		return ""
	}
	return t.Skills.Link
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
