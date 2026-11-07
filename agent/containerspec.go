package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/infra"
	"github.com/dmitriyb/faber/security"
)

// BoxSpec is everything the host needs to launch one box attempt: the
// resolved template from the IR (never re-read from YAML), the stringified
// input bindings, and the per-attempt switches. The security
// bindings (network, remote, identity, credentials) are composed separately
// by the pipeline via security.BindingSet and spliced into the returned
// ContainerSpec's Bindings field.
type BoxSpec struct {
	// RunID and NodeID form the deterministic container name together with
	// Attempt (1-based).
	RunID   string
	NodeID  string
	Attempt int

	// Template is the step's resolved template from the IR.
	Template *config.ResolvedTemplate

	// Image is the tag the infra image builder produced for the template.
	Image string

	// Inputs maps bound input slot names to stringified values, typed per
	// the slot's declaration. The reserved "repo" slot additionally selects
	// the remote binding host-side; the box itself keys clone behavior off
	// the remote env's presence.
	Inputs map[string]string

	// SkillsLink is the agent-specific $HOME-relative path the box symlinks to
	// the baked /faber/skills tree (emitted as FABER_SKILLS_LINK). Empty means
	// no skills leg — no env, and the box's skills phase is a no-op. The tree
	// itself, like the entry binary and the hook scripts, is image content:
	// the image builder bakes them, the run spec mounts nothing for them.
	SkillsLink string

	// AgentCLI is the agent CLI binary name inside the box (from the
	// template's package set). Opaque user config: faber defaults no vendor.
	// May be omitted when the template's env already carries FABER_AGENT_CLI.
	AgentCLI string

	// Model, Effort, ExtraInstruction and MaxBudget are pass-throughs to the
	// agent invocation; empty omits them. Model and Effort come from the
	// template's mandatory fields, so engine callers always set them.
	Model            string
	Effort           string
	ExtraInstruction string
	MaxBudget        string

	// GitName and GitEmail set the box committer identity (per-role registry
	// state; the name falls back to faber-<identity>, the email never falls
	// back — gated steps abort without one).
	GitName  string
	GitEmail string

	// GitCache, when set, is the container path of a read-only git object cache
	// (a pre-warmed volume) the box clones with --reference-if-able. Empty means
	// a plain full clone.
	GitCache string

	// Sessions turns transcript capture on: the box is told the container
	// path of the profile's session_dir (FABER_SESSIONS_DIR) and ships
	// everything under it to the host when the step ends. Set by the pipeline
	// only when the per-run sessions toggle is on AND the template's resolved
	// profile names a session_dir; setting it against a template whose profile
	// names none is a refusal (the pair-set guard). Off means no env,
	// byte-identical run specs.
	Sessions bool

	// Check marks a check run: emitted as FABER_CHECK=1, the box skips the
	// agent and fills unwritten outputs with placeholders.
	Check bool
}

// BuildContainerSpec assembles the engine half of the box run contract: container
// name, engine mounts (the workspace volume, the tmpfs writables, template
// volumes; faber-box, hooks and skills are image content and the result dir
// is a tmpfs the box ships out), the FABER_* environment, and the entry argv. No secret ever enters the
// environment — credentials arrive only through the security bindings the
// caller splices in afterwards. All violations are collected, never
// first-error.
func BuildContainerSpec(spec BoxSpec) (infra.ContainerSpec, error) {
	var errs []error
	check := func(cond bool, msg string) {
		if !cond {
			errs = append(errs, errors.New(msg))
		}
	}
	check(spec.Template != nil, "template: required")
	check(spec.Image != "", "image: required")
	check(spec.RunID != "", "run id: required")
	check(spec.NodeID != "", "node id: required")
	check(spec.Attempt >= 1, "attempt: must be >= 1")
	if spec.Template == nil {
		return infra.ContainerSpec{}, fmt.Errorf("agent: build run spec: %w", errors.Join(errs...))
	}
	tpl := spec.Template
	check(tpl.Skill != "", "template skill: required")
	check(spec.AgentCLI != "" || tpl.Env[contract.EnvAgentCLI] != "",
		"agent cli: required (no vendor default); set BoxSpec.AgentCLI or the template env "+contract.EnvAgentCLI)
	// Session capture needs its container path from the profile; capture
	// against a template whose profile names no session_dir has nothing to
	// ship. The container path must stay strictly inside HOME — config
	// validation guarantees it for engine callers, re-checked here for direct
	// BoxSpec callers (the box ships that tree, and a path outside its HOME
	// tmpfs would ship something that is not the harness's session).
	sessionsContainer := ""
	if spec.Sessions {
		switch {
		case tpl.Invoke == nil || tpl.Invoke.SessionDir == "":
			errs = append(errs, errors.New("sessions: dir set but the template's invocation profile names no session_dir"))
		default:
			sessionsContainer = path.Join(contract.ContainerHome, tpl.Invoke.SessionDir)
			if !pathUnder(sessionsContainer, contract.ContainerHome) || sessionsContainer == contract.ContainerHome {
				errs = append(errs, fmt.Errorf("sessions: profile session_dir %q does not resolve to a proper subpath of %s", tpl.Invoke.SessionDir, contract.ContainerHome))
				sessionsContainer = ""
			}
		}
	}

	// Template env is opaque user config for the box's own toolchain — it may
	// never set engine- or security-owned names (the FABER_ namespace, the
	// agent socket), or it could redirect hooks, enable TOFU, or
	// point the box at an arbitrary remote. FABER_AGENT_CLI is the one
	// documented exception.
	for _, key := range slices.Sorted(maps.Keys(tpl.Env)) {
		if key == contract.EnvAgentCLI {
			continue
		}
		if contract.EngineOwnedEnv(key) {
			errs = append(errs, fmt.Errorf("template env %q: engine- or security-owned name (only %s may be set here)", key, contract.EnvAgentCLI))
		}
	}
	// Template volumes may never shadow an engine mount or a binding mount —
	// docker's last-mount-wins would sever the result channel or substitute
	// hooks, secrets, or the agent socket.
	for _, host := range slices.Sorted(maps.Keys(tpl.Volumes)) {
		if r := reservedMountConflict(tpl.Volumes[host]); r != "" {
			errs = append(errs, fmt.Errorf("template volume %q -> %q: overlaps the reserved container path %q", host, tpl.Volumes[host], r))
		}
	}

	for _, slot := range slices.Sorted(maps.Keys(spec.Inputs)) {
		if _, ok := tpl.Inputs[slot]; !ok {
			errs = append(errs, fmt.Errorf("input %q: not declared by template %s", slot, tpl.Name))
		}
	}
	var required []string
	for _, slot := range slices.Sorted(maps.Keys(tpl.Inputs)) {
		if !tpl.Inputs[slot].Required {
			continue
		}
		required = append(required, slot)
		if spec.Inputs[slot] == "" {
			errs = append(errs, fmt.Errorf("input %q: required by template %s and absent or empty", slot, tpl.Name))
		}
	}
	schema, err := json.Marshal(tpl.Output)
	if err != nil {
		errs = append(errs, fmt.Errorf("output schema: %w", err))
	}
	// The resolved invocation dialect rides to the box as JSON; nil means the
	// template declared no invoke: block and the box's built-in default (the
	// same value config compiles defaults from) applies — no env var emitted,
	// so pre-profile IRs produce byte-identical run specs.
	var invoke []byte
	if tpl.Invoke != nil {
		if invoke, err = json.Marshal(tpl.Invoke); err != nil {
			errs = append(errs, fmt.Errorf("invoke profile: %w", err))
		}
	}
	if len(errs) > 0 {
		return infra.ContainerSpec{}, fmt.Errorf("agent: build run spec for %s: %w", spec.NodeID, errors.Join(errs...))
	}

	env := map[string]string{}
	maps.Copy(env, tpl.Env)
	env[contract.EnvContractVersion] = strconv.Itoa(contract.ContractVersion)
	env[contract.EnvSkill] = tpl.Skill
	env[contract.EnvResultDir] = contract.ContainerResultDir
	env[contract.EnvBundleDir] = contract.ContainerBundleDir
	env[contract.EnvOutputSchema] = string(schema)
	env[contract.EnvAttempt] = strconv.Itoa(spec.Attempt)
	if len(required) > 0 {
		env[contract.EnvRequiredInputs] = strings.Join(required, ",")
	}
	if slots := slices.Sorted(maps.Keys(tpl.Inputs)); len(slots) > 0 {
		env[contract.EnvInputSlots] = strings.Join(slots, ",")
	}
	setIf := func(key, val string) {
		if val != "" {
			env[key] = val
		}
	}
	setIf(contract.EnvIdentity, tpl.Identity)
	setIf(contract.EnvInvokeProfile, string(invoke))
	setIf(contract.EnvAgentCLI, spec.AgentCLI)
	setIf(contract.EnvModel, spec.Model)
	setIf(contract.EnvEffort, spec.Effort)
	setIf(contract.EnvExtraInstruction, spec.ExtraInstruction)
	setIf(contract.EnvMaxBudget, spec.MaxBudget)
	setIf(contract.EnvGitName, spec.GitName)
	setIf(contract.EnvGitEmail, spec.GitEmail)
	setIf(contract.EnvGitCache, spec.GitCache)
	setIf(contract.EnvSkillsLink, spec.SkillsLink)
	setIf(contract.EnvSessionsDir, sessionsContainer)
	if tpl.AgentOptional {
		env[contract.EnvAgentOptional] = "1"
	}
	if spec.Check {
		env[contract.EnvCheck] = "1"
	}
	// The box starts as root and drops to a run user. The value is the host
	// user's uid:gid for continuity; nothing host-owned is shared with the
	// box any more, so inside the container it is an id and nothing more.
	env[contract.EnvRunUID] = strconv.Itoa(os.Getuid())
	env[contract.EnvRunGID] = strconv.Itoa(os.Getgid())
	for slot, val := range spec.Inputs {
		env[contract.InputEnv(slot)] = val
	}

	// The entry binary, the hook scripts and the skills tree are image content
	// (the image builder bakes them under /faber), and nothing host-owned is
	// shared with the box: the writable engine mounts are the disk-backed
	// clone volume and the tmpfs scratch (result, bundle, tmp, home). All
	// arrive root-owned; the box's preamble chowns them to the run user and
	// drops privileges before any hook or agent runs. The result tmpfs is
	// shipped to the host over the box's stdout at the end of the attempt.
	mounts := []infra.Mount{
		{Kind: infra.KindVolume, Container: contract.ContainerWorkspace},
		{Kind: infra.KindTmpfs, Container: contract.ContainerResultDir},
		{Kind: infra.KindTmpfs, Container: contract.ContainerBundleDir},
		{Kind: infra.KindTmpfs, Container: "/tmp"},
		{Kind: infra.KindTmpfs, Container: contract.ContainerHome},
	}
	for _, host := range slices.Sorted(maps.Keys(tpl.Volumes)) {
		mounts = append(mounts, infra.Mount{Host: host, Container: tpl.Volumes[host]})
	}

	return infra.ContainerSpec{
		Name:      ContainerName(spec.RunID, spec.NodeID, spec.Attempt),
		Image:     spec.Image,
		Resources: tpl.Resources,
		Mounts:    mounts,
		Env:       env,
		Entry:     []string{contract.ContainerEntry},
	}, nil
}

// reservedContainerPaths are the container paths owned by the box run
// contract and the security bindings; template volumes may not touch them.
var reservedContainerPaths = []string{
	"/faber", // result dir, bundle dir, handoff dir, agent socket, hooks, skills, entry binary
	security.ContainerSecretsDir,
	contract.ContainerWorkspace,
	contract.ContainerHome, // the box's HOME tmpfs — last-mount-wins would replace it
	"/tmp",                 // the box's scratch tmpfs
}

// reservedMountConflict returns the reserved path a container mount path
// overlaps (as, under, or above it), or "" when the mount is safe.
func reservedMountConflict(container string) string {
	p := path.Clean(container)
	for _, r := range reservedContainerPaths {
		if pathUnder(p, r) || pathUnder(r, p) {
			return r
		}
	}
	return ""
}

// pathUnder reports whether a equals b or lies beneath it.
func pathUnder(a, b string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/")
}

// ContainerName is the deterministic per-attempt container name:
// faber-<slug(run-id)>-<slug(step-id)>-<id-hash>-a<attempt>. The slugs keep
// the name readable; the short hash of the raw (run-id, node-id) pair keeps
// it injective — slugging alone would collide "task/x" with "task-x" and
// blur the run/node boundary.
func ContainerName(runID, nodeID string, attempt int) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + nodeID))
	return "faber-" + slug(runID) + "-" + slug(nodeID) + "-" + hex.EncodeToString(sum[:4]) + "-a" + strconv.Itoa(attempt)
}

// slug maps an id onto the docker name alphabet: lowercase alphanumerics
// with every other run of characters collapsed to one dash.
func slug(s string) string {
	var sb strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			dash = false
		default:
			if !dash && sb.Len() > 0 {
				sb.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.TrimSuffix(sb.String(), "-")
}
