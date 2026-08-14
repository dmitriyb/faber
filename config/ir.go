package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// IRVersion is the major IR version this package emits. Desugaring is
// byte-stable across faber versions within a major IR version.
// 2: ResolvedTemplate gained the mandatory model/effort fields.
const IRVersion = 2

// Node kinds. The kind field is an open discriminator by design: deferred
// non-agent step kinds (human-approval gates, wait/poll steps, pure-command
// steps) will be new kinds with the same port/edge grammar. No such kind ships
// in the first pass; KindSelector is the one synthetic form the frontend
// cannot write (post-loop coalescing).
const (
	KindAgent       = "agent"
	KindSubWorkflow = "sub-workflow"
	KindGenerate    = "generate"
	KindSelector    = "selector"
)

// IR is the engine's backend contract: a uniform, acyclic, fully explicit
// graph. YAML is for people; the IR is for the machine. It embeds everything
// downstream needs, so desugaring never happens twice per run.
type IR struct {
	IRVersion int    `json:"ir_version"`
	Workflow  string `json:"workflow"`
	Nodes     []Node `json:"nodes"` // sorted by ID
	Edges     []Edge `json:"edges"` // sorted by (To, ToPort, From, FromPort)
}

// Node is one IR graph node. IDs are path-like and human-readable
// ("task/implement", "task/review-cycle@2/fix"); iteration instances carry @i.
// Stable ids are what make the journal key meaningful across resumes.
type Node struct {
	ID        string                 `json:"id"`
	Kind      string                 `json:"kind"`
	Template  *ResolvedTemplate      `json:"template,omitempty"` // agent nodes
	Sub       *IR                    `json:"sub,omitempty"`      // sub-workflow nodes
	Gen       *GenSpec               `json:"gen,omitempty"`      // generate nodes
	Sel       *SelSpec               `json:"sel,omitempty"`      // selector nodes
	Bindings  map[string]BindingDesc `json:"bindings"`           // literals, params, items
	When      *CondSpec              `json:"when,omitempty"`
	Retry     int                    `json:"retry,omitempty"`
	OnFailure string                 `json:"on_failure,omitempty"`
	Tools     []string               `json:"tools,omitempty"` // declared tool needs (subset-checked)
}

// Edge is a graph edge. A data edge carries a typed value between ports and is
// derived from a ${steps.X.field} reference, never authored. An ordering edge
// (empty ports) carries nothing and comes only from depends_on and loop
// chaining. The scheduler treats both identically for readiness.
type Edge struct {
	From     string `json:"from"`
	FromPort string `json:"from_port,omitempty"` // empty => ordering edge
	To       string `json:"to"`
	ToPort   string `json:"to_port,omitempty"`
}

// IsOrdering reports whether e is a pure ordering edge.
func (e Edge) IsOrdering() bool { return e.FromPort == "" && e.ToPort == "" }

// CondSpec carries a condition as compile-checkable CEL source plus the IR
// node ids whose results the expression reads (extracted at desugar time).
// Step references are rewritten to steps["<node-id>"].field form so the key a
// condition reads under is exactly the dep node id.
type CondSpec struct {
	CEL  string   `json:"cel"`
	Deps []string `json:"deps"` // node IDs the expression reads, sorted
}

// BindingDesc is a non-edge binding, resolved at node instantiation.
type BindingDesc struct {
	Kind  string `json:"kind"`            // literal | param | item
	Value any    `json:"value,omitempty"` // literal payload
	Type  string `json:"type,omitempty"`  // literal YAML type
	Name  string `json:"name,omitempty"`  // param name
	Field string `json:"field,omitempty"` // item field
}

// Binding descriptor kinds.
const (
	BindLiteral = "literal"
	BindParam   = "param"
	BindItem    = "item"
)

// GenSpec is a generate node's payload: the data-source command, the target
// workflow kept by name (expansion is run time), and the item binding template.
type GenSpec struct {
	Source   string                 `json:"source"`
	Command  string                 `json:"command"`
	Args     []string               `json:"args,omitempty"`
	Workflow string                 `json:"workflow"`
	Bindings map[string]BindingDesc `json:"bindings"`
}

// SelSpec is the post-loop selector: an alias node whose output ports mirror a
// loop-body step's outputs and whose value resolves at run time to the newest
// executed iteration's result. Exhausted encodes the loop-exhaustion failure
// rule (all iterations ran and the until predicate is still false at the final
// candidate => the selector settles failed); its semantics are consumed by the
// failure module — this package only wires it. The selector runs no container.
type SelSpec struct {
	Step       string    `json:"step"`       // the body step this selector aliases
	Candidates []string  `json:"candidates"` // node IDs, newest iteration first
	Exhausted  *CondSpec `json:"exhausted,omitempty"`
}

// ResolvedTemplate embeds everything the executor needs (image spec, resolved
// hook paths, identity, resources, I/O schemas) so the run phase never re-reads
// the YAML. Its shape is preserved by the config library redesign except the
// skills leg, which widens from a single {dir, link} to ResolvedSkills.
type ResolvedTemplate struct {
	Name      string            `json:"name"`
	Packages  []string          `json:"packages"`
	Overlay   string            `json:"overlay,omitempty"`
	Pin       *PinDef           `json:"pin,omitempty"` // optional resolved nixpkgs pin; nil ⇒ omitted ⇒ IR byte-stable when absent
	Identity  string            `json:"identity,omitempty"`
	Resources ResourceDef       `json:"resources"`
	Runtime   string            `json:"runtime,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Volumes   map[string]string `json:"volumes,omitempty"`
	Skill     string            `json:"skill"`
	Model     string            `json:"model"`  // mandatory opaque agent pass-through (rendered via the profile's model flag)
	Effort    string            `json:"effort"` // mandatory opaque agent pass-through (rendered via the profile's effort flag)
	// Invoke is the concrete invocation dialect the box expands, resolved at
	// desugar (built-in default ⊕ named profile ⊕ inline overrides — the box
	// never re-derives a field default). omitempty: a template without an
	// invoke: block emits IR bytes identical to before the field existed, and
	// changing a profile changes the IR hash — resume then refuses (drift)
	// rather than silently re-dialecting a resumed step's agent.
	Invoke *ResolvedInvoke `json:"invoke,omitempty"`
	// AgentOptional carries the template's agent-skippable opt-in into the
	// box env contract. omitempty: a template without the opt-in emits IR
	// bytes identical to before the field existed, and flipping it changes
	// the IR hash — resume then refuses (drift) rather than silently
	// changing whether a resumed step's agent runs.
	AgentOptional bool                `json:"agent_optional,omitempty"`
	Hooks         HookSet             `json:"hooks"`
	Skills        *ResolvedSkills     `json:"skills,omitempty"` // optional skill-definition delivery; nil = no skills leg
	Inputs        map[string]ParamDef `json:"inputs"`
	Output        map[string]FieldDef `json:"output"`
}

// Skill placement modes of an invocation profile.
const (
	SkillModePrefix = "prefix" // the skill is injected into the prompt via {skill}
	SkillModeFlag   = "flag"   // the skill rides its own argument pair (SkillFlag, skill)
)

// ResolvedInvoke is a concrete, fully-defaulted agent-CLI invocation dialect —
// every field final, no layering left. It serializes twice under the same JSON
// tags: into the IR on ResolvedTemplate.Invoke, and over the box env contract
// as the FABER_INVOKE_PROFILE payload the in-container expander consumes.
type ResolvedInvoke struct {
	Subcommand     []string `json:"subcommand,omitempty"`  // argv tokens between the CLI and the prompt
	PromptFlag     string   `json:"prompt_flag,omitempty"` // "" ⇒ the prompt is a bare positional argument
	SkillMode      string   `json:"skill_mode"`            // prefix | flag
	SkillFlag      string   `json:"skill_flag,omitempty"`  // the skill's argument name in flag mode
	PromptTemplate string   `json:"prompt_template"`       // over {skill} {body} {extra}
	FixedFlags     []string `json:"fixed_flags,omitempty"` // literal argv tail always appended
	ModelFlag      string   `json:"model_flag,omitempty"`  // "" ⇒ the pair is never emitted
	EffortFlag     string   `json:"effort_flag,omitempty"` // "" ⇒ never emitted
	BudgetFlag     string   `json:"budget_flag,omitempty"` // "" ⇒ never emitted
	SessionDir     string   `json:"session_dir,omitempty"` // $HOME-relative transcript dir; "" ⇒ no session capture
	ResumeArgv     []string `json:"resume_argv,omitempty"` // session-resuming argv; empty ⇒ shell-only re-entry
}

// DefaultInvoke is the anonymous built-in invocation dialect: its field values
// reproduce the invocation the engine emitted before profiles existed,
// byte-for-byte (pinned by a table test in the agent module). It lives in
// config because both altitudes consume it — desugar seeds profile layering
// with it, and the box falls back to it when FABER_INVOKE_PROFILE is absent —
// so the compiled profile and the direct-invocation fallback can never drift.
func DefaultInvoke() ResolvedInvoke {
	return ResolvedInvoke{
		PromptFlag:     "-p",
		SkillMode:      SkillModePrefix,
		PromptTemplate: "/{skill}\n\n{body}{extra}",
		FixedFlags:     []string{"--permission-mode", "bypassPermissions"},
		ModelFlag:      "--model",
		EffortFlag:     "--effort",
		BudgetFlag:     "--max-budget-usd",
	}
}

// Violations reports the invocation-profile rules a concrete profile breaks,
// each as a profile-relative field path plus message: the skill-mode enum, a
// prompt template that can carry the context bundle ({body}), and skill
// injection happening exactly once — via the prompt in prefix mode, via the
// flag pair in flag mode. The single source for both altitudes: the Loader's
// validate-time checks (prefixed with the declaring field path) and the box's
// env phase (a run-time guard for direct sequencer invocations, so a
// rule-breaking hand-authored FABER_INVOKE_PROFILE fail-stops instead of
// expanding nonsense).
func (ri ResolvedInvoke) Violations() []FieldError {
	var out []FieldError
	add := func(path, format string, args ...any) {
		out = append(out, FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
	}
	switch ri.SkillMode {
	case SkillModePrefix:
		if !strings.Contains(ri.PromptTemplate, "{skill}") {
			add("prompt_template", "skill_mode %q requires {skill} in the prompt template (the skill would be unreachable)", SkillModePrefix)
		}
		if ri.SkillFlag != "" {
			add("skill_flag", "only meaningful with skill_mode %q", SkillModeFlag)
		}
	case SkillModeFlag:
		if ri.SkillFlag == "" {
			add("skill_flag", "required with skill_mode %q", SkillModeFlag)
		}
		if strings.Contains(ri.PromptTemplate, "{skill}") {
			add("prompt_template", "skill_mode %q forbids {skill} in the prompt template (the skill is injected exactly once, via the flag pair); set a prompt_template without it", SkillModeFlag)
		}
	default:
		add("skill_mode", "unknown mode %q (legal: %s, %s)", ri.SkillMode, SkillModePrefix, SkillModeFlag)
	}
	if !strings.Contains(ri.PromptTemplate, "{body}") {
		add("prompt_template", "must contain {body} (the prompt cannot carry the context bundle without it)")
	}
	// The session dialect: the capture bind lands at $HOME/<session_dir>, so
	// the path must stay strictly inside the box HOME — never absolute, never
	// climbing out, never HOME itself (a bind over the whole tmpfs).
	if d := ri.SessionDir; d != "" {
		switch {
		case strings.HasPrefix(d, "/"):
			add("session_dir", "must be relative to $HOME, not absolute: %q", d)
		case hasDotDotSegment(d):
			add("session_dir", "must not contain a %q path segment: %q", "..", d)
		case path.Clean(d) == ".":
			add("session_dir", "must name a proper subpath of $HOME, not $HOME itself: %q", d)
		}
	}
	if len(ri.ResumeArgv) > 0 && ri.SessionDir == "" {
		add("resume_argv", "requires a session_dir in the same effective profile (there is no session it could resume)")
	}
	for i, tok := range ri.ResumeArgv {
		if tok == "" {
			add(fmt.Sprintf("resume_argv[%d]", i), "empty argv token")
		}
	}
	return out
}

// ResolvedSkills is the resolved source side of a template's skills leg. Exactly
// one of Sources / Root is populated: the named form yields Sources (an ordered,
// name-deduped set of single-skill trees the pipeline run-prep stager farms
// under <name>/); the inline {dir, link} form yields Root (a skills-root of
// <name>/SKILL.md subtrees the stager mounts directly). The delivered contract
// downstream is unchanged (one /faber/skills tree + one FABER_SKILLS_LINK); only
// the source representation widens so run-prep can stage N dirs into that tree.
type ResolvedSkills struct {
	Sources []SkillSource `json:"sources,omitempty"` // NAMED form: ordered, name-deduped {Name, Dir}
	Root    string        `json:"root,omitempty"`    // INLINE form: a skills-ROOT, mounted DIRECTLY
	Primary string        `json:"primary"`           // == template.skill
	Link    string        `json:"link"`              // in-box $HOME-relative discovery path
}

// SkillSource is one named skill's resolved single-skill tree.
type SkillSource struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// EncodeIR emits the canonical serialized form: nodes sorted by id, edges by
// (to, to_port, from, from_port), JSON object keys in fixed struct order, map
// keys sorted (encoding/json guarantee), HTML escaping off, two-space indent,
// trailing newline. The same YAML bytes always produce these same IR bytes.
func EncodeIR(ir *IR) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ir); err != nil {
		return nil, fmt.Errorf("config: encode IR: %w", err)
	}
	return buf.Bytes(), nil
}

// HashIR returns the hex SHA-256 of the canonical IR bytes. Resume
// compatibility is defined as "same bytes out of the desugaring pipeline";
// this is the hash the journal header records.
func HashIR(ir *IR) (string, error) {
	b, err := EncodeIR(ir)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// canonicalize sorts nodes and edges (recursively through sub-workflow graphs)
// and normalizes nil binding maps so emission is deterministic.
func canonicalize(ir *IR) {
	if ir.Nodes == nil {
		ir.Nodes = []Node{}
	}
	if ir.Edges == nil {
		ir.Edges = []Edge{}
	}
	sort.Slice(ir.Nodes, func(i, j int) bool { return ir.Nodes[i].ID < ir.Nodes[j].ID })
	sort.Slice(ir.Edges, func(i, j int) bool {
		a, b := ir.Edges[i], ir.Edges[j]
		if a.To != b.To {
			return a.To < b.To
		}
		if a.ToPort != b.ToPort {
			return a.ToPort < b.ToPort
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.FromPort < b.FromPort
	})
	for i := range ir.Nodes {
		if ir.Nodes[i].Bindings == nil {
			ir.Nodes[i].Bindings = map[string]BindingDesc{}
		}
		if ir.Nodes[i].Gen != nil && ir.Nodes[i].Gen.Bindings == nil {
			ir.Nodes[i].Gen.Bindings = map[string]BindingDesc{}
		}
		if ir.Nodes[i].Sub != nil {
			canonicalize(ir.Nodes[i].Sub)
		}
	}
}
