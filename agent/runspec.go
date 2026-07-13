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
// input bindings, and the host paths of the mounted pieces. The security
// bindings (network, remote, identity, credentials) are composed separately
// by the pipeline via security.BindingSet and spliced into the returned
// RunSpec's Bindings field.
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

	// ResultDir is the host directory mounted read-write as the box's result
	// directory — the container boundary's only writable engine mount.
	ResultDir string

	// EntryBinary is the host path of the statically built faber-box binary,
	// bind-mounted read-only and set as the container command.
	EntryBinary string

	// ContextHook and PreludeHook are host paths of the template's opaque
	// hook executables (resolved from Template.Hooks by the caller); empty
	// means the phase is a no-op inside the box.
	ContextHook string
	PreludeHook string

	// AgentCLI is the agent CLI binary name inside the box (from the
	// template's package set). Opaque user config: faber defaults no vendor.
	// May be omitted when the template's env already carries FABER_AGENT_CLI.
	AgentCLI string

	// Effort, ExtraInstruction and MaxBudget are pass-throughs to the agent
	// invocation; empty omits them.
	Effort           string
	ExtraInstruction string
	MaxBudget        string

	// GitName and GitEmail override the box's committer defaults.
	GitName  string
	GitEmail string

	// GitCache, when set, is the container path of a read-only git object cache
	// (a pre-warmed volume) the box clones with --reference-if-able. Empty means
	// a plain full clone.
	GitCache string
}

// BuildRunSpec assembles the engine half of the box run contract: container
// name, engine mounts (result dir, hooks, entry binary, template volumes),
// the FABER_* environment, and the entry argv. No secret ever enters the
// environment — credentials arrive only through the security bindings the
// caller splices in afterwards. All violations are collected, never
// first-error.
func BuildRunSpec(spec BoxSpec) (infra.RunSpec, error) {
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
	check(spec.ResultDir != "", "result dir: required")
	check(spec.EntryBinary != "", "entry binary: required")
	if spec.Template == nil {
		return infra.RunSpec{}, fmt.Errorf("agent: build run spec: %w", errors.Join(errs...))
	}
	tpl := spec.Template
	check(tpl.Skill != "", "template skill: required")
	check(spec.AgentCLI != "" || tpl.Env[contract.EnvAgentCLI] != "",
		"agent cli: required (no vendor default); set BoxSpec.AgentCLI or the template env "+contract.EnvAgentCLI)

	// Template env is opaque user config for the box's own toolchain — it may
	// never set engine- or security-owned names (the FABER_ namespace, the
	// forwarded agent socket), or it could redirect hooks, enable TOFU, or
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
	// hooks, secrets, or the forwarded socket.
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
	if len(errs) > 0 {
		return infra.RunSpec{}, fmt.Errorf("agent: build run spec for %s: %w", spec.NodeID, errors.Join(errs...))
	}

	env := map[string]string{}
	maps.Copy(env, tpl.Env)
	env[contract.EnvSkill] = tpl.Skill
	env[contract.EnvResultDir] = contract.ContainerResultDir
	env[contract.EnvBundleDir] = contract.ContainerBundleDir
	env[contract.EnvOutputSchema] = string(schema)
	env[contract.EnvAttempt] = strconv.Itoa(spec.Attempt)
	if len(required) > 0 {
		env[contract.EnvRequiredInputs] = strings.Join(required, ",")
	}
	setIf := func(key, val string) {
		if val != "" {
			env[key] = val
		}
	}
	setIf(contract.EnvIdentity, tpl.Identity)
	setIf(contract.EnvAgentCLI, spec.AgentCLI)
	setIf(contract.EnvEffort, spec.Effort)
	setIf(contract.EnvExtraInstruction, spec.ExtraInstruction)
	setIf(contract.EnvMaxBudget, spec.MaxBudget)
	setIf(contract.EnvGitName, spec.GitName)
	setIf(contract.EnvGitEmail, spec.GitEmail)
	setIf(contract.EnvGitCache, spec.GitCache)
	// The box starts as root and drops to the host user: its uid:gid so the
	// files the box writes to the result bind stay host-owned.
	env[contract.EnvRunUID] = strconv.Itoa(os.Getuid())
	env[contract.EnvRunGID] = strconv.Itoa(os.Getgid())
	for slot, val := range spec.Inputs {
		env[contract.InputEnv(slot)] = val
	}

	mounts := []infra.Mount{
		{Host: spec.ResultDir, Container: contract.ContainerResultDir},
		{Host: spec.EntryBinary, Container: contract.ContainerEntry, ReadOnly: true},
	}
	if spec.ContextHook != "" {
		mounts = append(mounts, infra.Mount{
			Host: spec.ContextHook, Container: contract.ContainerHooksDir + "/" + contract.HookContext, ReadOnly: true,
		})
	}
	if spec.PreludeHook != "" {
		mounts = append(mounts, infra.Mount{
			Host: spec.PreludeHook, Container: contract.ContainerHooksDir + "/" + contract.HookPrelude, ReadOnly: true,
		})
	}
	// Writable engine mounts: the disk-backed clone volume and the tmpfs scratch
	// (bundle, tmp, home). All arrive root-owned; the box's preamble chowns them
	// to the run user and drops privileges before any hook or agent runs.
	mounts = append(mounts,
		infra.Mount{Kind: infra.KindVolume, Container: contract.ContainerWorkspace},
		infra.Mount{Kind: infra.KindTmpfs, Container: contract.ContainerBundleDir},
		infra.Mount{Kind: infra.KindTmpfs, Container: "/tmp"},
		infra.Mount{Kind: infra.KindTmpfs, Container: contract.ContainerHome},
	)
	for _, host := range slices.Sorted(maps.Keys(tpl.Volumes)) {
		mounts = append(mounts, infra.Mount{Host: host, Container: tpl.Volumes[host]})
	}

	return infra.RunSpec{
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
	"/faber", // result dir, bundle dir, hooks, entry binary
	security.ContainerSecretsDir,
	security.ContainerAgentSocket,
	contract.ContainerWorkspace,
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
