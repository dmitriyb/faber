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

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Config is the root of the orchestrator.yaml struct tree. Structs map 1:1 to
// YAML sections; nothing else in the engine parses YAML.
//
// A Config is composed from a project assembly (the substrate) plus five named
// component libraries. The project file carries the substrate and an include:
// list; each included file contributes library entries. Assemble merges every
// included file into one flat Config before validation runs (see load.go).
type Config struct {
	Version     int                    `yaml:"version"`
	Include     []string               `yaml:"include,omitempty"`     // partial-config files, declarer-relative
	Network     NetworkDef             `yaml:"network,omitempty"`     // substrate: root-only
	Remote      RemoteDef              `yaml:"remote,omitempty"`      // substrate: root-only
	Credentials CredentialsDef         `yaml:"credentials,omitempty"` // substrate: root-only
	Identities  map[string]IdentityDef `yaml:"identities,omitempty"`  // substrate: root-only
	Images      map[string]ImageDef    `yaml:"images,omitempty"`      // library: union-merged
	Skills      map[string]SkillDef    `yaml:"skills,omitempty"`      // library: union-merged
	Hooks       map[string]HookDef     `yaml:"hooks,omitempty"`       // library: union-merged
	Templates   map[string]TemplateDef `yaml:"templates,omitempty"`   // library: union-merged
	Workflows   map[string]WorkflowDef `yaml:"workflows,omitempty"`   // library: union-merged
}

// ImageDef is a named pure Nix toolset — structurally today's BuildDef. A
// template references it by name via image:; the toolset IS the environment.
type ImageDef struct {
	Packages []string `yaml:"packages,omitempty"` // pinned Nix package names
	Overlay  string   `yaml:"overlay,omitempty"`  // optional overlay of derivations, declarer-relative
}

// SkillDef is a named skill definition: ONE skill's tree, SKILL.md at its root
// (no <name>/ wrapper). Named-mode templates deliver a set of these.
type SkillDef struct {
	Dir string `yaml:"dir,omitempty"` // host dir of one skill's tree, declarer-relative
}

// HookDef is a named hook executable. A template's hooks.<field> may name one
// of these (bare) instead of an inline path.
type HookDef struct {
	Path string `yaml:"path,omitempty"` // opaque hook script path, declarer-relative
}

// NetworkDef is the workflow-level network binding. Exactly one egress mode
// must be set when a network is configured: Proxy (the allow-listing egress
// proxy) or Nftables (the baked in-image rule set, whose root entrypoint
// needs NET_ADMIN). Making the choice explicit means a capability grant can
// never happen by omission.
type NetworkDef struct {
	Name     string   `yaml:"name,omitempty"`
	Proxy    string   `yaml:"proxy,omitempty"`
	NoProxy  []string `yaml:"no_proxy,omitempty"`
	Nftables bool     `yaml:"nftables,omitempty"`
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

// TemplateDef is a composition node, dual-mode: an aspect (image/hooks/skills/
// identity) is expressed EITHER by named reference into a library OR by the
// current inline form. The Loader enforces per-aspect exclusivity and reference
// existence; the Desugarer resolves both to the same ResolvedTemplate.
//
// build: round-trips mechanically through its struct tag (a plain mapping);
// skills: alone needs custom UnmarshalYAML because its value is a sequence (a
// name list) OR a mapping (inline {dir,link}), which one struct field cannot
// decode both of.
type TemplateDef struct {
	// named-reference form
	Image      string `yaml:"image,omitempty"`       // ref -> Images   (xor Build)
	Identity   string `yaml:"identity,omitempty"`    // ref -> Identities (alias for Run.Identity; xor it)
	SkillsLink string `yaml:"skills_link,omitempty"` // in-box $HOME-relative discovery path; required when Skills is the named list

	// shared / inline forms
	Build  *BuildDef           `yaml:"build,omitempty"` // inline {packages, overlay} (xor Image)
	Run    RunDef              `yaml:"run,omitempty"`
	Skill  string              `yaml:"skill,omitempty"` // the /<skill> prompt token; a Skills-library ref ONLY in named mode, else free-form
	Hooks  HookSet             `yaml:"hooks,omitempty"` // each value: a hook NAME (bare) or a PATH (has separator / begins ./~//)
	Skills SkillsRef           `yaml:"-"`               // sequence of names OR inline {dir,link}; set by UnmarshalYAML
	Inputs map[string]ParamDef `yaml:"inputs,omitempty"`
	Output map[string]FieldDef `yaml:"output,omitempty"`
}

// SkillsRef captures the two skills: node kinds without ambiguity: a YAML
// sequence yields Names (named form); a YAML mapping yields Inline (current
// {dir,link}). At most one of Names/Inline is populated; both empty = no skills
// leg (current default). The desugarer projects it into the two-variant
// resolved view (Names -> ResolvedSkills.Sources; Inline -> ResolvedSkills.Root).
type SkillsRef struct {
	Names  []string   // named form: skills: [implement, go-expert] — each a Skills-library ref
	Inline *SkillsDef // inline form: skills: {dir, link}
}

// SkillsDef is the inline skill-delivery form (unchanged). When present both
// fields are required. NOTE the level difference vs SkillDef.Dir: this Dir is a
// skills-ROOT holding <name>/SKILL.md subtrees (mounted DIRECTLY at
// /faber/skills), whereas SkillDef.Dir is a SINGLE skill's tree the named-mode
// stager wraps under <name>/. Conflating them double-nests the inline case.
type SkillsDef struct {
	Dir  string `yaml:"dir,omitempty"`  // skills-root: children are <name>/SKILL.md subtrees; declarer-relative
	Link string `yaml:"link,omitempty"` // in-box path relative to $HOME where THIS agent discovers skills; opaque to faber
}

// BuildDef is the inline toolset form; structurally identical to ImageDef. The
// package list IS the environment.
type BuildDef struct {
	Packages []string `yaml:"packages,omitempty"`
	Overlay  string   `yaml:"overlay,omitempty"`
}

// UnmarshalYAML is the second custom unmarshaller (beside StepDef). It decodes
// the plain fields via a shadow type, then handles the one node-kind
// disambiguation struct tags cannot: skills: as a sequence (named list) versus
// a mapping (inline {dir,link}). build: is decoded mechanically by its tag.
func (t *TemplateDef) UnmarshalYAML(n *yaml.Node) error {
	type raw TemplateDef // shadow: decodes every tagged field, drops the UnmarshalYAML method (no recursion)
	var r raw
	if err := n.Decode(&r); err != nil {
		return err
	}
	*t = TemplateDef(r)

	// skills: sequence => named list; mapping => inline {dir,link}.
	if sn := childNode(n, "skills"); sn != nil {
		switch sn.Kind {
		case yaml.SequenceNode:
			if err := sn.Decode(&t.Skills.Names); err != nil {
				return err
			}
		case yaml.MappingNode:
			t.Skills.Inline = new(SkillsDef)
			if err := sn.Decode(t.Skills.Inline); err != nil {
				return err
			}
		case yaml.ScalarNode:
			// A null or empty scalar (`skills:` with no value) means the aspect is
			// absent — leave the skills leg zero, exactly as if the key were omitted.
			// Any other scalar is a genuine type error.
			if sn.Tag != "!!null" && sn.Value != "" {
				return fmt.Errorf("skills: must be a list of names or a {dir, link} mapping")
			}
		default:
			return fmt.Errorf("skills: must be a list of names or a {dir, link} mapping")
		}
	}
	return nil
}

// MarshalYAML re-emits the skills: aspect (which carries the yaml:"-" tag) so a
// loaded Config round-trips: a name list marshals back to a sequence, an inline
// leg back to a mapping. Every other field round-trips through its struct tag.
func (t TemplateDef) MarshalYAML() (any, error) {
	type raw TemplateDef // shadow drops MarshalYAML, so the tagged fields render mechanically
	node := &yaml.Node{}
	if err := node.Encode(raw(t)); err != nil {
		return nil, err
	}
	switch {
	case t.Skills.Names != nil:
		if err := setMapChild(node, "skills", t.Skills.Names); err != nil {
			return nil, err
		}
	case t.Skills.Inline != nil:
		if err := setMapChild(node, "skills", t.Skills.Inline); err != nil {
			return nil, err
		}
	}
	return node, nil
}

// childNode returns a mapping node's value node for key, or nil.
func childNode(n *yaml.Node, key string) *yaml.Node {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// setMapChild appends (or replaces) a key/value pair on a mapping node, encoding
// value into a fresh value node.
func setMapChild(n *yaml.Node, key string, value any) error {
	vn := &yaml.Node{}
	if err := vn.Encode(value); err != nil {
		return err
	}
	if existing := childNode(n, key); existing != nil {
		*existing = *vn
		return nil
	}
	kn := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
	n.Content = append(n.Content, kn, vn)
	return nil
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
