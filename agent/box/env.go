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

	// HooksDir, SecretsDir and WorkspaceDir default to the fixed container
	// paths; the env overrides exist for running the sequencer as a plain
	// process (the lifecycle tests).
	HooksDir     string
	SecretsDir   string
	WorkspaceDir string

	// RunUID and RunGID are the host user's uid:gid the privileged preamble
	// chowns the writable mounts to and drops privileges into. 0 means no drop
	// (already non-root, e.g. a gateless local invocation).
	RunUID int
	RunGID int

	// AgentSocketGID is the supplementary gid the identity binding granted via
	// --group-add so the box can reach the forwarded, root-owned agent socket
	// (the macOS VM case); the preamble's setgroups must keep it alongside
	// RunGID or the grant is silently stripped. -1 means none: the env var was
	// absent or blank, or failed to parse (0 is a valid gid — macOS's socket
	// group — so it cannot double as the sentinel).
	AgentSocketGID int

	// GitCache is a read-only git object cache path; when set the clone adds
	// --reference-if-able so it borrows objects instead of duplicating history.
	GitCache string

	// SkillsLink is the $HOME-relative path the skills phase symlinks to the
	// read-only skills mount (contract.ContainerSkillsDir). Empty means the
	// template declares no skills leg and the phase is a no-op.
	SkillsLink string

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

	// rawSchema, rawAttempt, rawTOFU, rawContract, rawAgentOptional and
	// rawInvoke hold the undecoded values for the env phase.
	rawSchema        string
	rawAttempt       string
	rawTOFU          string
	rawContract      string
	rawAgentOptional string
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
		GitCache:         get(contract.EnvGitCache),
		SkillsLink:       get(contract.EnvSkillsLink),
		SecretsStdin:     get(contract.EnvSecretsStdin) == "1",
		rawSchema:        get(contract.EnvOutputSchema),
		rawAttempt:       get(contract.EnvAttempt),
		rawTOFU:          get(security.EnvHostKeyTOFU),
		rawContract:      get(contract.EnvContractVersion),
		rawAgentOptional: get(contract.EnvAgentOptional),
		rawInvoke:        get(contract.EnvInvokeProfile),
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
	// -1 sentinel: absent, blank, or unparseable all mean "no socket gid to
	// preserve" — 0 is a valid gid (macOS's socket group) and must not be
	// confused with "unset".
	env.AgentSocketGID = -1
	if raw := get(security.EnvAgentSocketGID); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			env.AgentSocketGID = n
		}
	}
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
	// binary exactly — a mismatch means the host's box_bin points at a stale or
	// foreign sequencer and no phase below may run on guessed semantics.
	// Absence is tolerated (direct invocations, lifecycle tests).
	if e.rawContract != "" && e.rawContract != strconv.Itoa(contract.ContractVersion) {
		errs = append(errs, fmt.Errorf("%s: host speaks contract v%s, this faber-box implements v%d — the host config's box_bin points at a mismatched binary",
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
