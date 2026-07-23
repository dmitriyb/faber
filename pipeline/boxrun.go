package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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
	// A reused attempt dir (a resumed run's attempt numbering restarts; a
	// crashed launch may have deposited files) can hold a stale result.json
	// that ExtractResult would adopt as this attempt's outcome. Clear it so
	// whatever the extractor reads was written by this attempt's container.
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
	// Run-prep stages the resolved skills leg into the single /faber/skills mount:
	// a named Sources set is copied into a per-attempt tree of real files, an
	// inline Root is mounted directly. The staging tree is torn down on the way out.
	skillsHost, skillsCleanup, err := stageSkills(box.Template.Skills, attemptDir)
	if err != nil {
		return BoxResult{}, fmt.Errorf("pipeline: box %s: stage skills: %w", box.NodeID, err)
	}
	defer skillsCleanup()
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
		SkillsDir:   skillsHost,
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
// skill stager applies to names): a traversal like "../../.." would otherwise
// bind an arbitrary host directory into the operator's re-entry container.
func adaptResult(rec agent.Result, box BoxAttempt, runRes infra.RunResult, log *slog.Logger) failure.Result {
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
	case reasonSkippedCondition, reasonSkippedDependency, reasonDeferred, reasonCached:
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

// stageSkills collapses a resolved skills leg into the single host path bound
// read-only at /faber/skills. An inline Root is bound directly (byte-identical
// to today's single-dir mount — the root already has the <name>/SKILL.md shape);
// a named Sources set is COPIED into a per-attempt tree <stage>/<name> of real
// files under attemptDir. The copy (rather than a symlink farm) is load-bearing:
// <stage> is bind-mounted read-only into the container, and a symlink's target
// would be a host path that is not mounted — it would dangle inside the box and
// /faber/skills/<name>/SKILL.md would silently vanish. A nil leg yields "" — no
// /faber/skills mount, today's no-skills behavior. Whatever it returns is set as
// the one Mount{Host} for /faber/skills, so infra's argv builder still sees
// exactly one read-only bind. The returned cleanup removes any staging tree on
// teardown.
func stageSkills(rs *config.ResolvedSkills, attemptDir string) (hostPath string, cleanup func(), err error) {
	noop := func() {}
	if rs == nil {
		return "", noop, nil
	}
	if rs.Root != "" {
		return rs.Root, noop, nil // inline: mount the skills-root directly, no <name> wrapper
	}
	stage := filepath.Join(attemptDir, "skills")
	// Crash-safe: a reused session dir (interactive re-entry) can carry a leftover
	// stage tree from a hard crash; clear it before restaging.
	if err := os.RemoveAll(stage); err != nil {
		return "", noop, err
	}
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", noop, err
	}
	for _, src := range rs.Sources {
		// Belt-and-suspenders: config.Validate already rejects unsafe skill names,
		// but staging joins src.Name onto stage as a path component, so re-check
		// here — a bypassed or hand-built ResolvedSkills must never write outside
		// the per-attempt tree.
		if !safeSegment(src.Name) {
			_ = os.RemoveAll(stage)
			return "", noop, fmt.Errorf("stage skill %q: unsafe name (must be a single path segment)", src.Name)
		}
		if err := copyTree(filepath.Join(stage, src.Name), src.Dir); err != nil {
			_ = os.RemoveAll(stage)
			return "", noop, fmt.Errorf("stage skill %q: %w", src.Name, err)
		}
	}
	return stage, func() { _ = os.RemoveAll(stage) }, nil
}

// safeSegment reports whether name is a single filesystem path component safe to
// join onto the stage dir: no separator, not "." / ".." / absolute, no leading
// dot. Mirrors config.safeName (the validate-time gate) so staging enforces the
// same discipline even if validation is ever bypassed.
func safeSegment(name string) bool {
	if name == "" || name == "." || name == ".." || filepath.IsAbs(name) {
		return false
	}
	if name[0] == '.' || name[0] == '~' {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// copyTree recursively copies the source tree at src into dst as REAL files, so
// the result survives the read-only bind into the container (a symlink would
// point at an unmounted host path and dangle inside the box). The box's run user
// is non-root and a :ro mount cannot be chowned by the box preamble, so every
// node is made world-readable: directories 0o755, files 0o644. Non-regular
// entries (nested symlinks, devices) are skipped — only real files travel.
func copyTree(dst, src string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(target, path)
	})
}

// copyFile streams src into a fresh world-readable dst (0o644) rather than
// slurping it whole, so a large skill file never spikes host memory. No size
// ceiling is imposed: the skills tree is operator-authored.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// skillsLink reads the template's optional skills-discovery path, returning ""
// when the template declares no skills leg (absence = current behavior).
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
