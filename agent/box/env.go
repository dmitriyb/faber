package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/security"
)

// BoxEnv is the box's decoded environment contract. ParseEnv only collects —
// validation belongs to the sequencer's env phase, so a violation still
// funnels through the fail-stop path and leaves a handoff record.
type BoxEnv struct {
	Skill    string
	AgentCLI string
	Identity string

	ResultDir string
	BundleDir string

	// RemoteURL is the complete gateway clone URL (the security module
	// splices the repo input host-side). Absence means a gateless step: the
	// hostkey/clone/signing phases are skipped by contract.
	RemoteURL string
	HostKey   string

	// TOFU is the sandbox-only trust-on-first-use opt-in; set by the env
	// phase only for the exact contract value "1" — anything else is an
	// env-contract violation, never a silent accept-new.
	TOFU bool

	// Inputs maps slot env tokens (the FABER_INPUT_ suffix) to values.
	Inputs map[string]string

	// RequiredInputs lists the slot names FABER_REQUIRED_INPUTS declares.
	RequiredInputs []string

	// Slots lists ALL declared input slot names (FABER_INPUT_SLOTS), so the
	// fail-stop handoff can be recorded slot-keyed — the slot→token mapping
	// is lossy in reverse. Empty on a pre-versioning or direct invocation.
	Slots []string

	// Schema is the decoded output schema; set by the env phase.
	Schema contract.OutputSchema

	// Attempt is the decoded attempt ordinal; set by the env phase (1 when
	// FABER_ATTEMPT is absent).
	Attempt int

	Model            string
	Effort           string
	ExtraInstruction string
	MaxBudget        string

	// Invoke is the concrete invocation dialect the agent phase expands,
	// decoded from FABER_INVOKE_PROFILE by the env phase. Absent or empty
	// means the built-in default (config.DefaultInvoke) — the tolerated
	// absence that keeps direct sequencer invocations and pre-profile hosts
	// working without a contract-version bump. Malformed JSON, or a profile
	// violating the template rules, is an env-contract violation — never a
	// silent fallback to a dialect the host did not send.
	Invoke config.ResolvedInvoke

	GitName  string
	GitEmail string

	// HooksDir, SecretsDir, WorkspaceDir and HandoffDir default to the fixed
	// container paths; the env overrides exist for running the sequencer as a
	// plain process (the lifecycle tests).
	HooksDir     string
	SecretsDir   string
	WorkspaceDir string
	HandoffDir   string

	// RunUID and RunGID are the host user's uid:gid the privileged preamble
	// chowns the writable mounts to and drops privileges into. 0 means no drop
	// (already non-root, e.g. a gateless local invocation).
	RunUID int
	RunGID int

	// AgentSocket is SSH_AUTH_SOCK: the box's own agent socket, which the
	// host serves through `faber-box relay` a moment after the container
	// starts; the clone phase waits for it. Empty means no identity.
	AgentSocket string

	// GitCache is a read-only git object cache path; when set the clone adds
	// --reference-if-able so it borrows objects instead of duplicating history.
	GitCache string

	// SkillsLink is the $HOME-relative path the skills phase symlinks to the
	// baked skills tree (contract.ContainerSkillsDir). Empty means the
	// template declares no skills leg and the phase is a no-op.
	SkillsLink string

	// Volumes are the container mount points of the template's named
	// volumes (FABER_VOLUMES): root-owned when docker creates one, so the
	// preamble chowns each to the run user, non-recursively.
	Volumes []string

	// RemoteDir is where faber-box seed-remote writes a check run's bare
	// repository (the check's volume at /faber/remote; overridable for
	// plain-process tests).
	RemoteDir string

	// SessionsDir is the container path (under HOME) of the harness's
	// session transcripts: shipped as sessions/<rel> frames at the end of
	// the attempt, and the root faber-box receive writes a re-entry's
	// transcript under. Empty means capture is off.
	SessionsDir string

	// SecretsStdin reports FABER_SECRETS_STDIN=1: file-mode tokens arrive as a
	// JSON payload on the box's stdin, and the secrets phase materializes them
	// into the /run/secrets tmpfs before the origin-agnostic env export. Unset
	// means the phase never touches stdin.
	SecretsStdin bool

	// AgentOptional reports FABER_AGENT_OPTIONAL=1: the template declares
	// itself agent-skippable, so a prelude's skip request in the bundle
	// sidecar takes effect. Set by the env phase only for the exact contract
	// value "1" — anything else is an env-contract violation, never a silent
	// opt-in.
	AgentOptional bool

	// Check reports FABER_CHECK=1: a check run — the agent phase is a no-op
	// and the result phase fills unwritten outputs with placeholders. Set by
	// the env phase only for the exact contract value "1".
	Check bool

	// rawSchema, rawAttempt, rawTOFU, rawContract, rawAgentOptional,
	// rawCheck and rawInvoke hold the undecoded values for the env phase.
	rawSchema        string
	rawAttempt       string
	rawTOFU          string
	rawContract      string
	rawAgentOptional string
	rawCheck         string
	rawInvoke        string
}

// ParseEnv decodes the box environment. It never fails: the env phase
// validates, so main can always construct the Box and every violation is
// reported through the structured fail-stop path.
func ParseEnv(environ []string) *BoxEnv {
	get := func(key string) string {
		for _, kv := range environ {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				return v
			}
		}
		return ""
	}
	env := &BoxEnv{
		Skill:            get(contract.EnvSkill),
		AgentCLI:         get(contract.EnvAgentCLI),
		Identity:         get(contract.EnvIdentity),
		ResultDir:        get(contract.EnvResultDir),
		BundleDir:        get(contract.EnvBundleDir),
		RemoteURL:        get(security.EnvRemoteURL),
		HostKey:          get(security.EnvHostKey),
		Inputs:           map[string]string{},
		Model:            get(contract.EnvModel),
		Effort:           get(contract.EnvEffort),
		ExtraInstruction: get(contract.EnvExtraInstruction),
		MaxBudget:        get(contract.EnvMaxBudget),
		GitName:          get(contract.EnvGitName),
		GitEmail:         get(contract.EnvGitEmail),
		HooksDir:         withDefault(get(contract.EnvHooksDir), contract.ContainerHooksDir),
		SecretsDir:       withDefault(get(contract.EnvSecretsDir), security.ContainerSecretsDir),
		WorkspaceDir:     withDefault(get(contract.EnvWorkspaceDir), contract.ContainerWorkspace),
		HandoffDir:       withDefault(get(contract.EnvHandoffDir), contract.ContainerHandoffDir),
		RemoteDir:        withDefault(get(contract.EnvRemoteDir), contract.ContainerCheckRemote),
		GitCache:         get(contract.EnvGitCache),
		SkillsLink:       get(contract.EnvSkillsLink),
		SessionsDir:      get(contract.EnvSessionsDir),
		SecretsStdin:     get(contract.EnvSecretsStdin) == "1",
		AgentSocket:      get(security.EnvSSHAuthSock),
		rawSchema:        get(contract.EnvOutputSchema),
		rawAttempt:       get(contract.EnvAttempt),
		rawTOFU:          get(security.EnvHostKeyTOFU),
		rawContract:      get(contract.EnvContractVersion),
		rawAgentOptional: get(contract.EnvAgentOptional),
		rawCheck:         get(contract.EnvCheck),
		rawInvoke:        get(contract.EnvInvokeProfile),
	}
	for _, p := range strings.Split(get(contract.EnvVolumes), ":") {
		if p != "" {
			env.Volumes = append(env.Volumes, p)
		}
	}
	if raw := strings.TrimSpace(get(contract.EnvInputSlots)); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				env.Slots = append(env.Slots, name)
			}
		}
	}
	// Non-numeric or absent uid/gid parse to 0, which the preamble reads as "no
	// drop" — the same fail-safe as an already-non-root box.
	env.RunUID, _ = strconv.Atoi(get(contract.EnvRunUID))
	env.RunGID, _ = strconv.Atoi(get(contract.EnvRunGID))
	if req := strings.TrimSpace(get(contract.EnvRequiredInputs)); req != "" {
		for _, name := range strings.Split(req, ",") {
			if name = strings.TrimSpace(name); name != "" {
				env.RequiredInputs = append(env.RequiredInputs, name)
			}
		}
	}
	for _, kv := range environ {
		rest, ok := strings.CutPrefix(kv, contract.InputEnvPrefix)
		if !ok {
			continue
		}
		if token, val, ok := strings.Cut(rest, "="); ok && token != "" {
			env.Inputs[token] = val
		}
	}
	return env
}

// validate is the env phase's check: every violation collected, never
// first-error. On success the decoded schema and attempt are filled in.
func (e *BoxEnv) validate() error {
	var errs []error
	need := func(val, name string) {
		if val == "" {
			errs = append(errs, fmt.Errorf("%s: required and empty", name))
		}
	}
	need(e.Skill, contract.EnvSkill)
	need(e.AgentCLI, contract.EnvAgentCLI)
	need(e.ResultDir, contract.EnvResultDir)
	need(e.BundleDir, contract.EnvBundleDir)
	// Contract handshake: a host that stamps a version must match this
	// binary exactly — a mismatch means the image bakes a stale or foreign
	// faber-box and no phase below may run on guessed semantics.
	// Absence is tolerated (direct invocations, lifecycle tests).
	if e.rawContract != "" && e.rawContract != strconv.Itoa(contract.ContractVersion) {
		errs = append(errs, fmt.Errorf("%s: host speaks contract v%s, this faber-box implements v%d — a stale image: run faber build",
			contract.EnvContractVersion, e.rawContract, contract.ContractVersion))
	}
	for _, slot := range e.RequiredInputs {
		if e.Inputs[contract.SlotToken(slot)] == "" {
			errs = append(errs, fmt.Errorf("%s: required input slot %q is absent or empty", contract.InputEnv(slot), slot))
		}
	}
	switch e.rawTOFU {
	case "":
		// TOFU off.
	case "1":
		e.TOFU = true
	default:
		errs = append(errs, fmt.Errorf("%s: %q is not the contract value \"1\" — refusing to guess a trust policy", security.EnvHostKeyTOFU, e.rawTOFU))
	}
	switch e.rawAgentOptional {
	case "":
		// No opt-in: the agent always runs.
	case "1":
		e.AgentOptional = true
	default:
		errs = append(errs, fmt.Errorf("%s: %q is not the contract value \"1\" — refusing to guess whether the agent is skippable", contract.EnvAgentOptional, e.rawAgentOptional))
	}
	switch e.rawCheck {
	case "":
		// A real run.
	case "1":
		e.Check = true
	default:
		errs = append(errs, fmt.Errorf("%s: %q is not the contract value \"1\" — refusing to guess whether this is a check run", contract.EnvCheck, e.rawCheck))
	}
	// The invocation dialect: absent means the built-in default (the tolerated
	// absence serving direct invocations and pre-profile hosts); present means
	// a concrete profile that must decode and satisfy the shared profile rules
	// — never a silent fallback to a dialect the host did not send.
	if e.rawInvoke == "" {
		e.Invoke = config.DefaultInvoke()
	} else if err := json.Unmarshal([]byte(e.rawInvoke), &e.Invoke); err != nil {
		errs = append(errs, fmt.Errorf("%s: %v", contract.EnvInvokeProfile, err))
	} else {
		for _, fe := range e.Invoke.Violations() {
			errs = append(errs, fmt.Errorf("%s: %s: %s", contract.EnvInvokeProfile, fe.Path, fe.Msg))
		}
	}
	if e.HostKey != "" && e.TOFU {
		errs = append(errs, fmt.Errorf("%s and %s are mutually exclusive", security.EnvHostKey, security.EnvHostKeyTOFU))
	}
	schema, err := contract.ParseOutputSchema(e.rawSchema)
	if err != nil {
		errs = append(errs, fmt.Errorf("%s: %v", contract.EnvOutputSchema, err))
	} else {
		e.Schema = schema
	}
	e.Attempt = 1
	if e.rawAttempt != "" {
		n, err := strconv.Atoi(e.rawAttempt)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("%s: %q is not a positive integer", contract.EnvAttempt, e.rawAttempt))
		} else {
			e.Attempt = n
		}
	}
	return errors.Join(errs...)
}

// withDefault substitutes def for an empty value.
func withDefault(val, def string) string {
	if val == "" {
		return def
	}
	return val
}
