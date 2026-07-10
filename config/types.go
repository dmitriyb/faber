// Package config defines the orchestrator.yaml schema and its Go struct tree,
// loads and validates configuration, desugars the YAML frontend into the
// acyclic JSON IR, validates typed reference wiring over the IR, and provides
// the faber CLI entry points and structured logging.
//
// The schema is mechanism, not policy: all domain behavior enters through
// opaque config values (hook script paths, data-source commands, resolver
// commands, free-form param names). Nothing in this package interprets their
// contents.
package config

// Config is the root of the orchestrator.yaml struct tree. Structs map 1:1 to
// YAML sections; nothing else in the engine parses YAML.
type Config struct {
	Version     int                    `yaml:"version"`
	Network     NetworkDef             `yaml:"network,omitempty"`
	Remote      RemoteDef              `yaml:"remote,omitempty"`
	Credentials CredentialsDef         `yaml:"credentials,omitempty"`
	Identities  map[string]IdentityDef `yaml:"identities,omitempty"`
	Templates   map[string]TemplateDef `yaml:"templates,omitempty"`
	Workflows   map[string]WorkflowDef `yaml:"workflows,omitempty"`
}

// NetworkDef is the workflow-level network binding (name, proxy, no_proxy).
type NetworkDef struct {
	Name    string   `yaml:"name,omitempty"`
	Proxy   string   `yaml:"proxy,omitempty"`
	NoProxy []string `yaml:"no_proxy,omitempty"`
}

// RemoteDef is the gateway URL prefix plus host-key policy. Exactly one of
// HostKeyFile / TOFU must be set when URL is set.
type RemoteDef struct {
	URL         string `yaml:"url,omitempty"`
	HostKeyFile string `yaml:"host_key_file,omitempty"`
	TOFU        bool   `yaml:"tofu,omitempty"`
}

// CredentialsDef names the opaque resolver command and per-service handle modes.
type CredentialsDef struct {
	Resolver string                `yaml:"resolver,omitempty"`
	Services map[string]ServiceDef `yaml:"services,omitempty"`
}

// ServiceDef configures one credential handle. Mode is one of proxy|file|helper.
type ServiceDef struct {
	Mode     string `yaml:"mode,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}

// IdentityDef maps a role name to its resolver-interpreted key material source.
type IdentityDef struct {
	Key string `yaml:"key,omitempty"`
}

// TemplateDef describes one box, split exactly along the build/run boundary.
type TemplateDef struct {
	Build  BuildDef            `yaml:"build,omitempty"`
	Run    RunDef              `yaml:"run,omitempty"`
	Skill  string              `yaml:"skill,omitempty"`
	Hooks  HookSet             `yaml:"hooks,omitempty"`
	Inputs map[string]ParamDef `yaml:"inputs,omitempty"`
	Output map[string]FieldDef `yaml:"output,omitempty"`
}

// BuildDef pins the image: the package list IS the environment.
type BuildDef struct {
	Packages []string `yaml:"packages,omitempty"`
	Overlay  string   `yaml:"overlay,omitempty"`
}

// RunDef holds run-time knobs for the box.
type RunDef struct {
	Identity  string            `yaml:"identity,omitempty"`
	Resources ResourceDef       `yaml:"resources,omitempty"`
	Runtime   string            `yaml:"runtime,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
	Volumes   map[string]string `yaml:"volumes,omitempty"`
}

// ResourceDef holds container resource limits.
type ResourceDef struct {
	Memory string  `yaml:"memory,omitempty" json:"memory,omitempty"`
	CPUs   float64 `yaml:"cpus,omitempty" json:"cpus,omitempty"`
}

// HookSet holds the opaque hook script paths — the policy seams.
type HookSet struct {
	Context   string `yaml:"context,omitempty" json:"context,omitempty"`
	Prelude   string `yaml:"prelude,omitempty" json:"prelude,omitempty"`
	OnFailure string `yaml:"on_failure,omitempty" json:"on_failure,omitempty"`
}

// WorkflowDef declares a typed params interface, data sources for generate,
// and an ordered step list (order is documentation, edges are truth).
type WorkflowDef struct {
	Params  map[string]ParamDef  `yaml:"params,omitempty"`
	Sources map[string]SourceDef `yaml:"sources,omitempty"`
	Steps   []StepDef            `yaml:"steps,omitempty"`
}

// SourceDef is an opaque data-source command for generate.
type SourceDef struct {
	Command string   `yaml:"command,omitempty"`
	Args    []string `yaml:"args,omitempty"`
}

// ParamDef is the single typing vocabulary used end to end: workflow params,
// template input slots, and template output fields.
type ParamDef struct {
	Type     string   `yaml:"type,omitempty" json:"type"`
	Required bool     `yaml:"required,omitempty" json:"required,omitempty"`
	Default  any      `yaml:"default,omitempty" json:"default,omitempty"`
	Enum     []string `yaml:"enum,omitempty" json:"enum,omitempty"`
}

// FieldDef is the same vocabulary applied to output fields — one type system
// end to end, so a ${steps.X.field} reference checks against a slot without
// conversion rules.
type FieldDef = ParamDef

// StepDef is a tagged union with exactly three mutually exclusive forms:
// use-step (Use set), loop-step (Loop set), generate-step (Generate set).
// The Loader enforces that exactly one is set.
type StepDef struct {
	ID        string         `yaml:"id,omitempty"`
	Use       string         `yaml:"use,omitempty"`
	Loop      *LoopDef       `yaml:"loop,omitempty"`
	Generate  *GenerateDef   `yaml:"generate,omitempty"`
	With      map[string]any `yaml:"with,omitempty"`
	When      string         `yaml:"when,omitempty"`
	DependsOn []string       `yaml:"depends_on,omitempty"`
	Retry     int            `yaml:"retry,omitempty"`
	OnFailure string         `yaml:"on_failure,omitempty"`
	Tools     []string       `yaml:"tools,omitempty"`
}

// LoopDef is bounded loop sugar; Max is required so unrolling is always finite.
type LoopDef struct {
	Max   int       `yaml:"max,omitempty"`
	Until string    `yaml:"until,omitempty"`
	Steps []StepDef `yaml:"steps,omitempty"`
}

// GenerateDef fans a named workflow out over a data source at run time.
type GenerateDef struct {
	Source   string         `yaml:"source,omitempty"`
	Workflow string         `yaml:"workflow,omitempty"`
	With     map[string]any `yaml:"with,omitempty"`
}
