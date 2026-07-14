# Implementation: Schema structs and params typing

Covers SchemaTypes and Loader.

## Struct definitions (internal/config/types.go)

```go
type Config struct {
    Version     int                    `yaml:"version"`
    Network     NetworkDef             `yaml:"network"`
    Remote      RemoteDef              `yaml:"remote"`
    Credentials CredentialsDef         `yaml:"credentials"`
    Identities  map[string]IdentityDef `yaml:"identities"`
    Templates   map[string]TemplateDef `yaml:"templates"`
    Workflows   map[string]WorkflowDef `yaml:"workflows"`
}

type NetworkDef struct {
    Name    string   `yaml:"name"`
    Proxy   string   `yaml:"proxy"`
    NoProxy []string `yaml:"no_proxy"`
}

type RemoteDef struct {
    URL         string `yaml:"url"`
    HostKeyFile string `yaml:"host_key_file"`
    TOFU        bool   `yaml:"tofu"`
}

type CredentialsDef struct {
    Resolver string                `yaml:"resolver"`
    Services map[string]ServiceDef `yaml:"services"`
}

type ServiceDef struct {
    Mode     string `yaml:"mode"` // proxy | file | helper
    Endpoint string `yaml:"endpoint"`
}

type IdentityDef struct {
    Key string `yaml:"key"` // resolver-interpreted key material source
}

type TemplateDef struct {
    Build  BuildDef            `yaml:"build"`
    Run    RunDef              `yaml:"run"`
    Skill  string              `yaml:"skill"`
    Hooks  HookSet             `yaml:"hooks"`
    Skills *SkillsDef          `yaml:"skills"` // nil = no skills leg (current behavior)
    Inputs map[string]ParamDef `yaml:"inputs"`
    Output map[string]FieldDef `yaml:"output"`
}

// SkillsDef delivers skill definitions into the box. A pointer so absence is
// distinguishable from a zero value; when present, both fields are required.
type SkillsDef struct {
    Dir  string `yaml:"dir"`  // CWD-relative host dir of skill defs (SKILL.md tree), resolved like hook/overlay paths
    Link string `yaml:"link"` // in-box path relative to $HOME where THIS agent discovers skills; agent-specific, opaque to faber
}

type BuildDef struct {
    Packages []string `yaml:"packages"`
    Overlay  string   `yaml:"overlay"`
}

type RunDef struct {
    Identity  string            `yaml:"identity"`
    Resources ResourceDef       `yaml:"resources"`
    Runtime   string            `yaml:"runtime"`
    Env       map[string]string `yaml:"env"`     // plain box env (offline-dep knobs etc.)
    Volumes   map[string]string `yaml:"volumes"` // named volume -> mount path (pre-warmed caches)
}

type ResourceDef struct {
    Memory string  `yaml:"memory"` // docker memory string, e.g. "8g"
    CPUs   float64 `yaml:"cpus"`
}

type HookSet struct {
    Context   string `yaml:"context"`
    Prelude   string `yaml:"prelude"`
    OnFailure string `yaml:"on_failure"`
}

type WorkflowDef struct {
    Params  map[string]ParamDef  `yaml:"params"`
    Sources map[string]SourceDef `yaml:"sources"`
    Steps   []StepDef            `yaml:"steps"`
}

type SourceDef struct {
    Command string   `yaml:"command"`
    Args    []string `yaml:"args"`
}

type ParamDef struct {
    Type     string   `yaml:"type"` // string | int | bool | object
    Required bool     `yaml:"required"`
    Default  any      `yaml:"default"`
    Enum     []string `yaml:"enum"`
}
type FieldDef = ParamDef // one typing vocabulary end to end
```

## The StepDef union

```go
type StepDef struct {
    ID        string            `yaml:"id"`
    Use       string            `yaml:"use"`      // use-step
    Loop      *LoopDef          `yaml:"loop"`     // loop-step
    Generate  *GenerateDef      `yaml:"generate"` // generate-step
    With      map[string]any    `yaml:"with"`
    When      string            `yaml:"when"`
    DependsOn []string          `yaml:"depends_on"`
    Retry     int               `yaml:"retry"`
    OnFailure string            `yaml:"on_failure"`
}

type LoopDef struct {
    Max   int       `yaml:"max"`
    Until string    `yaml:"until"`
    Steps []StepDef `yaml:"steps"`
}

type GenerateDef struct {
    Source   string         `yaml:"source"`
    Workflow string         `yaml:"workflow"`
    With     map[string]any `yaml:"with"`
}
```

Exactly one of `Use`/`Loop`/`Generate` must be set; the Loader enforces this
(`workflows.task.steps[1]: step must have exactly one of use/loop/generate`).
`With` values stay `any` at load time; the reference grammar below types them.

## Reference parsing

One reference grammar shared by Loader (syntax), Desugarer (edge building), and
WiringChecker (resolution):

```go
type Ref struct {
    Root  RefRoot // Params | Steps | Item | Sources
    Name  string  // param name / step id / item field / source name
    Field string  // output field for Steps refs
}

// ParseBinding classifies a with: value.
func ParseBinding(v any) (Binding, error) // -> Literal | Reference
```

A `with:` string that contains `${` must be exactly one `${ref}` with no
surrounding text (no string templating in v1 — concatenation belongs in hooks).
`ParseRef` rejects unknown roots, empty segments, and `steps.X` without a field.

## Validation walk (internal/config/validate.go)

Single pass over the struct tree appending `fieldErr(path, msg)` to a slice,
`errors.Join` at the end. Path building mirrors YAML addressing
(`templates.review.output.verdict`). The check catalog is arch_loader.md's list;
each check is a small named function so tests assert them independently. Among
them: when `templates.<t>.skills` is present, both `skills.dir` and
`skills.link` must be non-empty (an all-or-nothing pair, field-pathed like the
rest); its contents are never read at load time — `dir` is an opaque path and
`link` an opaque agent-specific string.

## Params typing (internal/config/params.go)

```go
func CheckParams(decl map[string]ParamDef, supplied map[string]string) (map[string]TypedValue, error)
```

Parses `--param k=v` strings against declarations: type coercion from string form
(int/bool parsed, object accepts JSON), enum membership, required presence,
defaults applied. Returns the typed param environment consumed by the Desugarer's
binding descriptors and the executor. All violations joined, not first-error.
