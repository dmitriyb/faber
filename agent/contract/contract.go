// Package contract defines the box contract shared by the host half of the
// agent module and the in-container phase sequencer: the FABER_* environment
// names the host emits and the box consumes, the well-known in-container
// paths and file names, the attempt-record and handoff-record shapes, and the
// output-schema validation both sides apply.
//
// The package deliberately imports only the config module (for the shared
// I/O typing vocabulary) so the faber-box binary links nothing host-only.
// The remote/identity/credential names (FABER_REMOTE_URL, SSH_AUTH_SOCK,
// /run/secrets, ...) are owned by the security module and are not restated
// here.
package contract

import "strings"

// Environment variable names of the box env contract. The host side
// (BuildRunSpec in the agent package) emits them; the sequencer's env phase
// validates them. Absence of an optional name means the feature is off — no
// value is ever defaulted to a vendor-specific string.
const (
	// EnvSkill is the skill the agent phase activates (the leading
	// slash-command line of the prompt). Required.
	EnvSkill = "FABER_SKILL"

	// EnvAgentCLI is the agent CLI binary the agent phase executes. It must
	// resolve inside the box (the template's package set). Required — faber
	// hardcodes no agent vendor.
	EnvAgentCLI = "FABER_AGENT_CLI"

	// EnvIdentity is the step's identity role name, used for committer
	// defaults (faber-<identity>). Optional.
	EnvIdentity = "FABER_IDENTITY"

	// EnvResultDir is the host-mounted result directory: the box writes
	// result.json (and on failure handoff.json) there, the agent skill writes
	// output.json there. Required.
	EnvResultDir = "FABER_RESULT_DIR"

	// EnvBundleDir is the context-bundle directory the hooks fill. Required.
	EnvBundleDir = "FABER_BUNDLE_DIR"

	// EnvOutputSchema carries the template's declared output fields as JSON
	// (the OutputSchema type). Empty or absent means no declared outputs.
	EnvOutputSchema = "FABER_OUTPUT_SCHEMA"

	// EnvRequiredInputs is the comma-separated list of required input slot
	// names; the env phase checks each has a non-empty FABER_INPUT_* value.
	EnvRequiredInputs = "FABER_REQUIRED_INPUTS"

	// EnvAttempt is the 1-based attempt ordinal echoed into the record.
	EnvAttempt = "FABER_ATTEMPT"

	// EnvEffort, EnvExtraInstruction and EnvMaxBudget are pass-throughs to
	// the agent invocation; unset means the flag / trailer is omitted.
	EnvEffort           = "FABER_EFFORT"
	EnvExtraInstruction = "FABER_EXTRA_INSTRUCTION"
	EnvMaxBudget        = "FABER_MAX_BUDGET"

	// EnvGitName and EnvGitEmail override the committer identity configured
	// by the signing phase.
	EnvGitName  = "FABER_GIT_NAME"
	EnvGitEmail = "FABER_GIT_EMAIL"

	// EnvHooksDir, EnvSecretsDir and EnvWorkspaceDir relocate the fixed
	// container paths when the sequencer runs as a plain process (the box
	// lifecycle tests). The host never sets them; inside a container the
	// defaults below apply.
	EnvHooksDir     = "FABER_HOOKS_DIR"
	EnvSecretsDir   = "FABER_SECRETS_DIR"
	EnvWorkspaceDir = "FABER_WORKSPACE_DIR"

	// EnvRunUID and EnvRunGID are the host user's uid:gid the box's privileged
	// preamble chowns the writable mounts to and drops privileges into. Unset
	// or 0 means no drop (a gateless local invocation already running non-root).
	EnvRunUID = "FABER_RUN_UID"
	EnvRunGID = "FABER_RUN_GID"

	// EnvGitCache points at a read-only git object cache (a pre-warmed volume);
	// when set, the clone borrows objects via --reference-if-able. Empty = none.
	EnvGitCache = "FABER_GIT_CACHE"

	// InputEnvPrefix prefixes one variable per bound input slot:
	// FABER_INPUT_<SLOT>, the typed-inputs contract for hooks and agent.
	InputEnvPrefix = "FABER_INPUT_"
)

// Fixed in-container paths of the box run contract.
const (
	// ContainerResultDir is where the host mounts the result directory.
	ContainerResultDir = "/faber/result"

	// ContainerBundleDir is the context-bundle directory (container-local;
	// on fail-stop its content is snapshotted into the result dir).
	ContainerBundleDir = "/faber/bundle"

	// ContainerHooksDir is where hook executables are bind-mounted
	// read-only, one file per phase name (context, prelude).
	ContainerHooksDir = "/faber/hooks"

	// ContainerEntry is where the faber-box binary is bind-mounted read-only
	// and set as the container command.
	ContainerEntry = "/faber/bin/faber-box"

	// ContainerWorkspace is the parent directory of the gateway clone, mounted
	// as a disk-backed anonymous volume.
	ContainerWorkspace = "/workspace"

	// ContainerHome is the box's HOME: a tmpfs the preamble chowns to the run
	// user, exported as HOME before any hook or agent runs.
	ContainerHome = "/home/box"
)

// Well-known file names inside the bundle and result directories.
const (
	// ContextDoc is the mandatory prompt-body document of the context bundle.
	ContextDoc = "CONTEXT.md"

	// BundleEnvFile is the optional machine-readable bundle sidecar
	// (line-oriented KEY=VALUE, opaque values).
	BundleEnvFile = "bundle.env"

	// BranchKey is the one first-pass bundle.env convention: a declared
	// side-effect verified against the gateway after extraction.
	BranchKey = "BRANCH"

	// OutputFile is the skill-written typed output in the result directory.
	OutputFile = "output.json"

	// ResultFile is the single attempt record the box guarantees on every
	// exit path.
	ResultFile = "result.json"

	// HandoffFile is the structured fail-stop record beside the attempt
	// record; HandoffBundleDir is the bundle snapshot taken with it.
	HandoffFile      = "handoff.json"
	HandoffBundleDir = "handoff/bundle"

	// UsageFile is the advisory vendor usage sidecar the metering module's
	// reported tier reads; the box never interprets it.
	UsageFile = "usage.json"

	// HookContext and HookPrelude are the per-phase hook file names under
	// the hooks directory.
	HookContext = "context"
	HookPrelude = "prelude"
)

// InputEnv returns the environment variable carrying the named input slot:
// FABER_INPUT_<SLOT> with the slot name uppercased and "-" mapped to "_"
// (the same substitution the security module's env contract defines).
func InputEnv(slot string) string {
	return InputEnvPrefix + SlotToken(slot)
}

// SlotToken uppercases a slot name for env-var embedding, mapping "-" to "_".
func SlotToken(slot string) string {
	return strings.ToUpper(strings.ReplaceAll(slot, "-", "_"))
}

// EngineOwnedEnv reports whether an environment name belongs to the engine or
// security env contract: the whole FABER_ namespace (the box contract, input
// slots, and the security module's service/helper handle names) plus the
// forwarded agent socket variable. User-supplied environment (template env,
// bundle sidecars) may never set these — the contract is engine-owned.
func EngineOwnedEnv(key string) bool {
	return strings.HasPrefix(key, "FABER_") || key == "SSH_AUTH_SOCK"
}
