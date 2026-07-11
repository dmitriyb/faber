package box

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/dmitriyb/faber/agent/contract"
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

	// Schema is the decoded output schema; set by the env phase.
	Schema contract.OutputSchema

	// Attempt is the decoded attempt ordinal; set by the env phase (1 when
	// FABER_ATTEMPT is absent).
	Attempt int

	Effort           string
	ExtraInstruction string
	MaxBudget        string

	GitName  string
	GitEmail string

	// HooksDir, SecretsDir and WorkspaceDir default to the fixed container
	// paths; the env overrides exist for running the sequencer as a plain
	// process (the lifecycle tests).
	HooksDir     string
	SecretsDir   string
	WorkspaceDir string

	// rawSchema, rawAttempt and rawTOFU hold the undecoded values for the
	// env phase.
	rawSchema  string
	rawAttempt string
	rawTOFU    string
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
		Effort:           get(contract.EnvEffort),
		ExtraInstruction: get(contract.EnvExtraInstruction),
		MaxBudget:        get(contract.EnvMaxBudget),
		GitName:          get(contract.EnvGitName),
		GitEmail:         get(contract.EnvGitEmail),
		HooksDir:         withDefault(get(contract.EnvHooksDir), contract.ContainerHooksDir),
		SecretsDir:       withDefault(get(contract.EnvSecretsDir), security.ContainerSecretsDir),
		WorkspaceDir:     withDefault(get(contract.EnvWorkspaceDir), contract.ContainerWorkspace),
		rawSchema:        get(contract.EnvOutputSchema),
		rawAttempt:       get(contract.EnvAttempt),
		rawTOFU:          get(security.EnvHostKeyTOFU),
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
