# Implementation: Schema structs and params typing

Covers SchemaTypes and Loader.

## Struct definitions (internal/config/types.go)

```go
type Config struct {
    Version     int                    `yaml:"version"`
    Include     []string               `yaml:"include"`      // partial-config files, declarer-relative
    Network     NetworkDef             `yaml:"network"`      // substrate: root-only
    Remote      RemoteDef              `yaml:"remote"`       // substrate: root-only
    Credentials CredentialsDef         `yaml:"credentials"`  // substrate: root-only
    Identities  map[string]IdentityDef `yaml:"identities"`   // substrate: root-only
    Images      map[string]ImageDef    `yaml:"images"`       // library: union-merged
    Skills      map[string]SkillDef    `yaml:"skills"`       // library: union-merged
    Hooks       map[string]HookDef     `yaml:"hooks"`        // library: union-merged
    Templates   map[string]TemplateDef `yaml:"templates"`    // library: union-merged
    Workflows   map[string]WorkflowDef `yaml:"workflows"`    // library: union-merged
}

// The three new component libraries. Each entry is inert, opaque data; paths are
// resolved to absolute (declarer-relative) at load time, never read at load time.
type ImageDef struct {
    Packages []string `yaml:"packages"` // pinned Nix package names — identical to today's BuildDef
    Overlay  string   `yaml:"overlay"`  // optional overlay of derivations
    Pin      *PinDef  `yaml:"pin"`      // optional nixpkgs snapshot; nil ⇒ faber's compiled-in default
}
type SkillDef struct {
    Dir string `yaml:"dir"` // ONE skill's tree — SKILL.md at its root, no <name>/ wrapper
}
type HookDef struct {
    Path string `yaml:"path"` // opaque hook executable
}

// PinDef is an optional per-toolset nixpkgs snapshot, attachable to both ImageDef
// and the inline BuildDef (dual-mode parity). It is config's OWN type: config must
// NOT import the infra package (dependency direction is infra→config). The infra
// adapter maps config.PinDef → infra.NixpkgsPin at the config→infra boundary (see
// spec/infra/impl_nix_build.md). A fully-empty pin: {} normalizes to nil (absent).
// When present, BOTH fields are required and each is charset-validated at the Loader
// (user-supplied splice material); nil selects faber's compiled-in default pin.
type PinDef struct {
    Rev    string `yaml:"rev"`    // nixpkgs revision (commit or release tag); Loader-charset-checked
    SHA256 string `yaml:"sha256"` // fetchTarball hash; Loader-charset-checked
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

// TemplateDef is dual-mode: an aspect (image/hooks/skills/identity) is expressed
// EITHER by named reference into a library OR by the current inline form. The
// Loader enforces per-aspect exclusivity; the Desugarer resolves both to the same
// ResolvedTemplate. See arch_schema_types.md "Dual-mode".
type TemplateDef struct {
    // named-reference form (new)
    Image      string   `yaml:"image"`       // ref -> Images   (xor Build)
    Identity   string   `yaml:"identity"`    // ref -> Identities (alias for Run.Identity; xor it)
    SkillsLink string   `yaml:"skills_link"` // in-box $HOME-relative discovery path; required when Skills is the named list

    // shared / inline forms
    Build  *BuildDef           `yaml:"-"`      // inline {packages, overlay}; set by UnmarshalYAML (xor Image)
    Run    RunDef              `yaml:"run"`
    Skill  string              `yaml:"skill"`  // the /<skill> prompt token; a Skills-library ref ONLY in named mode, else free-form
    Hooks  HookSet             `yaml:"hooks"`  // each value: a hook NAME (bare) or a PATH (has separator)
    Skills SkillsRef           `yaml:"-"`      // sequence of names OR inline {dir,link}; set by UnmarshalYAML
    Inputs map[string]ParamDef `yaml:"inputs"`
    Output map[string]FieldDef `yaml:"output"`
}

// SkillsRef captures the two `skills:` node kinds without ambiguity: a YAML
// sequence yields Names (named form); a YAML mapping yields Inline (current
// {dir,link}). Custom UnmarshalYAML dispatches on the node kind; at most one of
// Names/Inline is populated. Empty = no skills leg (current default behavior).
// This unified unmarshal-time struct is the single source of truth; the desugarer
// projects it into the two-variant resolved view arch_schema_types.md "Skills
// assembly" describes (Names -> ResolvedSkills.Sources; Inline -> ResolvedSkills.Root).
type SkillsRef struct {
    Names  []string   // named form: skills: [implement, go-expert] — each a Skills-library ref
    Inline *SkillsDef // inline form: skills: {dir, link}
}

// SkillsDef is the inline skill-delivery form (unchanged). When present both
// fields are required. NOTE the level difference vs SkillDef.Dir: this Dir is a
// skills-ROOT holding <name>/SKILL.md subtrees (mounted DIRECTLY at /faber/skills),
// whereas SkillDef.Dir is a SINGLE skill's tree the named-mode stager wraps under
// <name>/. Conflating them double-nests the inline case; see impl_desugaring.md.
type SkillsDef struct {
    Dir  string `yaml:"dir"`  // skills-root: children are <name>/SKILL.md subtrees; declarer-relative
    Link string `yaml:"link"` // in-box path relative to $HOME where THIS agent discovers skills; opaque to faber
}

// BuildDef is the inline toolset form; structurally identical to ImageDef. Set on
// TemplateDef via UnmarshalYAML only when a `build:` key is present.
type BuildDef struct {
    Packages []string `yaml:"packages"`
    Overlay  string   `yaml:"overlay"`
    Pin      *PinDef  `yaml:"pin"` // optional nixpkgs snapshot; nil ⇒ faber's default (dual-mode parity with ImageDef)
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

// HookSet is unchanged. In a dual-mode template each value is EITHER a bare hook
// name (resolved against the Hooks library) OR a path (contains a separator, or
// begins with '.', '~', '/'). Resolution/classification happens at desugar; the
// struct itself is form-agnostic.
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

## Dual-mode template unmarshal (custom UnmarshalYAML)

`TemplateDef.UnmarshalYAML` is the second custom unmarshaller (beside `StepDef`).
It decodes the plain fields via a shadow type, then handles the two node-kind
disambiguations that struct tags cannot:

```go
func (t *TemplateDef) UnmarshalYAML(n *yaml.Node) error {
    type raw TemplateDef                 // shadow: decodes Image/Identity/Skill/Hooks/Run/Inputs/Output/skills_link
    var r raw
    if err := n.Decode(&r); err != nil { return err }
    *t = TemplateDef(r)

    // build: present ⇒ inline toolset form
    if bn := child(n, "build"); bn != nil {
        var b BuildDef
        if err := bn.Decode(&b); err != nil { return err }
        t.Build = &b
    }
    // skills: sequence ⇒ named list; mapping ⇒ inline {dir,link}
    if sn := child(n, "skills"); sn != nil {
        switch sn.Kind {
        case yaml.SequenceNode: sn.Decode(&t.Skills.Names)
        case yaml.MappingNode:  t.Skills.Inline = new(SkillsDef); sn.Decode(t.Skills.Inline)
        default: return fmt.Errorf("skills: must be a list of names or a {dir,link} mapping")
        }
    }
    return nil
}
```

`child(n, key)` finds a mapping node's value by key. No other field needs custom
handling — `image`, `identity`, `skills_link`, `hooks`, `run` round-trip through
struct tags. The Loader's exclusivity checks (below) then reject illegal
combinations; unmarshal itself only records what was written.

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

Single pass over the *assembled* struct tree (post-include-merge — see
arch_loader.md) appending `fieldErr(path, msg)` to a slice, `errors.Join` at the
end. Path building mirrors YAML addressing (`templates.review.output.verdict`).
The check catalog is arch_loader.md's list; each check is a small named function
so tests assert them independently. The dual-mode and library checks:

- **Per-aspect exclusivity.** `templates.<t>`: not both `image` and `build`; not
  both top-level `identity` and `run.identity`; `skills` is not simultaneously a
  named list and an inline mapping (guaranteed by unmarshal, re-asserted); when
  `skills` is the named list, `skills_link` is required and `run`/inline `skills`
  carry no `link`; when `skills` is inline `{dir,link}` **or empty/absent**,
  `skills_link` must be absent (a discovery path with no skills to discover is a
  violation). Each violation is field-pathed (`templates.review: image and build
  are mutually exclusive`).
- **Named-reference existence** (name level, resolved against the merged
  libraries): `templates.<t>.image ∈ images`; each bare-name `templates.<t>.hooks.*
  ∈ hooks`; `templates.<t>.identity` (top-level or `run.identity`) `∈ identities`.
  A bare hook name that resolves to no hook is a dangling-ref error, never a
  silent path.
- **Skills references — named mode only.** When `templates.<t>.skills` is the
  **named list**, every `templates.<t>.skills[*] ∈ skills` AND the primary
  `templates.<t>.skill ∈ templates.<t>.skills` (a box may only activate a skill it
  also delivers). When `skills` is **inline `{dir,link}` or absent**,
  `templates.<t>.skill` is a **free-form prompt token** — neither resolved against
  the `Skills` library nor membership-checked. This scoping is precisely what keeps
  every existing `skill:`-without-`skills:` config valid.
- **Inline skills pairing.** When `skills` is inline, both `skills.dir` and
  `skills.link` must be non-empty (the all-or-nothing pair, unchanged). Contents
  are never read at load time — `dir` is an opaque path, `link`/`skills_link` an
  opaque agent-specific string.
- **Pin normalization, completeness, and charset.** A fully-empty `pin: {}`
  unmarshals to a non-nil `&PinDef{}`; the Loader normalizes a **fully-empty** pin
  (both fields blank) back to `nil` (absent), because only a nil pin serializes to
  the `omitempty`-absent IR that keeps a pin-less toolset byte-stable — a present
  `&PinDef{}` would not. A pin with **exactly one** field set is *not* normalized:
  it is the completeness error below. When an `images.<name>.pin` or a template's
  inline `build.pin` is present (any field set), both `rev` and `sha256` must be
  non-empty — a partial pin is a field-pathed violation (`images.go-box.pin: rev and
  sha256 are both required`) — **and** each present value must match the splice-safe
  charset `^[A-Za-z0-9:+/=._-]+$`, because `rev`/`sha256` are spliced into infra's
  rendered `fetchTarball` call; an off-charset value is its own field-pathed
  violation (`images.go-box.pin.rev: invalid characters`). An absent (or normalized-
  away) `pin` is legal — it selects faber's default. Like every other check here,
  partial and off-charset pins are collected, not reported first-error-only. The pin
  values are otherwise opaque, never dereferenced or fetched at load time; the infra
  splice keeps a matching guard as defense-in-depth (see
  `spec/infra/impl_nix_build.md`).
- **Substrate placement.** A substrate key on an included (non-root) file is a
  violation (`<file>: included files may only contribute libraries`).

## Params typing (internal/config/params.go)

```go
func CheckParams(decl map[string]ParamDef, supplied map[string]string) (map[string]TypedValue, error)
```

Parses `--param k=v` strings against declarations: type coercion from string form
(int/bool parsed, object accepts JSON), enum membership, required presence,
defaults applied. Returns the typed param environment consumed by the Desugarer's
binding descriptors and the executor. All violations joined, not first-error.
