package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// maxUnrolledNodes is the sanity ceiling on the unrolled node count: configs
// that would exceed it are rejected with an error naming the offending loop
// rather than desugared unboundedly.
const maxUnrolledNodes = 10000

// Desugar compiles the schema-valid *Config into the canonical JSON IR for one
// workflow. It is a pure, deterministic function of its inputs: resolve reuse,
// unroll bounded loops into conditional chains, expand compact with: bindings
// into explicit typed edges and binding descriptors, and emit canonically. All
// policy questions (type correctness, acyclicity of the result) are left to
// CheckWiring — the Desugarer is a mechanical translator.
//
// The executed IR never contains a Loop op: bounded loops exist only in the
// frontend and arrive in the IR already unrolled.
func Desugar(cfg *Config, workflow string) (*IR, error) {
	if _, ok := cfg.Workflows[workflow]; !ok {
		return nil, &FieldError{Path: "workflows." + workflow, Msg: "workflow not declared"}
	}
	// A workflow that transitively includes itself cannot unroll: hard stop,
	// no partial IR.
	if err := checkWorkflowRefCycles(cfg, workflow); err != nil {
		return nil, err
	}
	d := &desugarer{cfg: cfg}
	ir := d.emitWorkflow(workflow, workflow)
	if len(d.errs) > 0 {
		return nil, errors.Join(d.errs...)
	}
	canonicalize(ir)
	return ir, nil
}

type desugarer struct {
	cfg       *Config
	errs      []error
	nodeCount int
}

func (d *desugarer) addf(path, format string, args ...any) {
	d.errs = append(d.errs, &FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// builder accumulates one graph (one workflow instance).
type builder struct {
	nodes []Node
	edges []Edge
	made  map[string]bool // node ids appended, for selector dedupe
}

func (b *builder) addNode(n Node) {
	if n.Bindings == nil {
		n.Bindings = map[string]BindingDesc{}
	}
	b.nodes = append(b.nodes, n)
	b.made[n.ID] = true
}

// level is one name-resolution scope: the workflow top level, or one loop-body
// iteration instance. Step names resolve innermost-out; a body step referenced
// from outside its loop resolves to that loop's selector node.
type level struct {
	parent *level
	prefix string               // node id prefix: "task" or "task/review-cycle@2"
	plain  map[string]string    // non-loop step id -> node id at this level
	loops  map[string]*loopDecl // loop step id -> declaration at this level
}

// loopDecl is the static description of one loop at the level where it is
// declared — enough to build selectors and depends_on targets on demand,
// before or after the loop body has been emitted.
type loopDecl struct {
	id     string
	prefix string // owning level prefix
	def    *LoopDef
	body   map[string]bool // direct non-loop body step ids
}

func (ld *loopDecl) instancePrefix(i int) string {
	return ld.prefix + "/" + ld.id + "@" + strconv.Itoa(i)
}

func newLevel(parent *level, prefix string, steps []StepDef) *level {
	l := &level{parent: parent, prefix: prefix, plain: map[string]string{}, loops: map[string]*loopDecl{}}
	for _, s := range steps {
		if s.ID == "" {
			continue
		}
		if s.Loop != nil && s.Use == "" && s.Generate == nil {
			body := map[string]bool{}
			for _, bs := range s.Loop.Steps {
				if bs.ID != "" && bs.Loop == nil {
					body[bs.ID] = true
				}
			}
			l.loops[s.ID] = &loopDecl{id: s.ID, prefix: prefix, def: s.Loop, body: body}
			continue
		}
		l.plain[s.ID] = prefix + "/" + s.ID
	}
	return l
}

// condPiece is one conjunct of a node condition. Gate pieces arrive already
// negation-wrapped ("!(U)") and are used verbatim; plain pieces are
// parenthesized when conjoined.
type condPiece struct {
	cel     string
	deps    []string
	wrapped bool
}

func combineCond(pieces []condPiece) *CondSpec {
	if len(pieces) == 0 {
		return nil
	}
	depSet := map[string]bool{}
	var exprs []string
	for _, p := range pieces {
		for _, dep := range p.deps {
			depSet[dep] = true
		}
		if p.wrapped || len(pieces) == 1 {
			exprs = append(exprs, p.cel)
		} else {
			exprs = append(exprs, "("+p.cel+")")
		}
	}
	deps := make([]string, 0, len(depSet))
	for dep := range depSet {
		deps = append(deps, dep)
	}
	sort.Strings(deps)
	return &CondSpec{CEL: strings.Join(exprs, " && "), Deps: deps}
}

// emitWorkflow desugars one workflow into a fresh graph, with all node ids
// under scope. Used for the entry workflow and recursively for inlined
// sub-workflow composition.
func (d *desugarer) emitWorkflow(wfName, scope string) *IR {
	wf := d.cfg.Workflows[wfName]
	b := &builder{made: map[string]bool{}}
	lvl := newLevel(nil, scope, wf.Steps)
	d.emitSteps(b, wf, wf.Steps, lvl, "workflows."+wfName+".steps", nil)
	return &IR{IRVersion: IRVersion, Workflow: wfName, Nodes: b.nodes, Edges: b.edges}
}

// emitSteps emits one step list within lvl. gates are condition pieces imposed
// from enclosing loop unrolling (settled-loop gates and the loop-step's own
// when:), conjoined onto every node emitted here.
func (d *desugarer) emitSteps(b *builder, wf WorkflowDef, steps []StepDef, lvl *level, yamlPath string, gates []condPiece) {
	for i, s := range steps {
		sp := fmt.Sprintf("%s[%d]", yamlPath, i)
		forms := 0
		for _, set := range []bool{s.Use != "", s.Loop != nil, s.Generate != nil} {
			if set {
				forms++
			}
		}
		if forms != 1 || s.ID == "" {
			d.addf(sp, "step must have an id and exactly one of use/loop/generate")
			continue
		}
		switch {
		case s.Loop != nil:
			d.unrollLoop(b, wf, s, lvl, sp, gates)
		case s.Generate != nil:
			d.emitGenerate(b, wf, s, lvl, sp, gates)
		default:
			if _, ok := d.cfg.Templates[s.Use]; ok {
				d.emitAgent(b, wf, s, lvl, sp, gates)
			} else if _, ok := d.cfg.Workflows[s.Use]; ok {
				d.emitSub(b, wf, s, lvl, sp, gates)
			} else {
				d.addf(sp+".use", "unknown template or workflow %q", s.Use)
			}
		}
	}
}

func (d *desugarer) emitAgent(b *builder, wf WorkflowDef, s StepDef, lvl *level, sp string, gates []condPiece) {
	nodeID := lvl.prefix + "/" + s.ID
	tmpl := d.resolveTemplate(s.Use, sp)
	node := Node{
		ID:        nodeID,
		Kind:      KindAgent,
		Template:  tmpl,
		Retry:     s.Retry,
		OnFailure: s.OnFailure,
		Tools:     append([]string(nil), s.Tools...),
	}
	node.Bindings = d.expandBindings(b, s.With, lvl, sp+".with", nodeID, false)
	node.When = d.stepCond(b, s.When, lvl, sp, gates)
	d.emitDependsOn(b, s, lvl, sp, nodeID)
	b.addNode(node)
	d.nodeCount++
}

func (d *desugarer) emitSub(b *builder, wf WorkflowDef, s StepDef, lvl *level, sp string, gates []condPiece) {
	nodeID := lvl.prefix + "/" + s.ID
	sub := d.emitWorkflow(s.Use, nodeID)
	node := Node{
		ID:        nodeID,
		Kind:      KindSubWorkflow,
		Sub:       sub,
		Retry:     s.Retry,
		OnFailure: s.OnFailure,
	}
	node.Bindings = d.expandBindings(b, s.With, lvl, sp+".with", nodeID, false)
	d.bakeScopeDefaults(node.Bindings, s.Use, s.With, sp)
	node.When = d.stepCond(b, s.When, lvl, sp, gates)
	d.emitDependsOn(b, s, lvl, sp, nodeID)
	b.addNode(node)
	d.nodeCount++
}

// bakeScopeDefaults adds a literal binding for every target-workflow param
// that declares a default and is not bound in with:, so sub-workflow and
// generate-instance scopes materialize declared defaults exactly like the
// run entry's CheckParams does for the root scope — a condition inside the
// scope can rely on every defaulted param being present.
func (d *desugarer) bakeScopeDefaults(bindings map[string]BindingDesc, target string, with map[string]any, sp string) {
	decl := d.cfg.Workflows[target].Params
	for _, name := range sortedKeys(decl) {
		p := decl[name]
		if p.Default == nil {
			continue
		}
		if _, bound := with[name]; bound {
			continue
		}
		def, err := normalizeValue(p.Default)
		if err != nil {
			d.addf(sp+".with."+name, "invalid default for param %q: %v", name, err)
			continue
		}
		bindings[name] = BindingDesc{Kind: BindLiteral, Value: def, Type: p.Type}
	}
}

func (d *desugarer) emitGenerate(b *builder, wf WorkflowDef, s StepDef, lvl *level, sp string, gates []condPiece) {
	nodeID := lvl.prefix + "/" + s.ID
	g := s.Generate
	src, ok := wf.Sources[g.Source]
	if !ok {
		d.addf(sp+".generate.source", "unknown source %q", g.Source)
		return
	}
	if _, ok := d.cfg.Workflows[g.Workflow]; !ok {
		d.addf(sp+".generate.workflow", "unknown workflow %q", g.Workflow)
		return
	}
	node := Node{
		ID:   nodeID,
		Kind: KindGenerate,
		Gen: &GenSpec{
			Source:   g.Source,
			Command:  src.Command,
			Args:     append([]string(nil), src.Args...),
			Workflow: g.Workflow,
			Bindings: d.expandBindings(b, g.With, lvl, sp+".generate.with", nodeID, true),
		},
		Retry:     s.Retry,
		OnFailure: s.OnFailure,
	}
	d.bakeScopeDefaults(node.Gen.Bindings, g.Workflow, g.With, sp+".generate")
	node.When = d.stepCond(b, s.When, lvl, sp, gates)
	d.emitDependsOn(b, s, lvl, sp, nodeID)
	b.addNode(node)
	d.nodeCount++
}

// unrollLoop expands loop {max: N, until: P, steps: B} into N copies of the
// body, B@1..B@N, chained linearly with ordering edges. Every node of
// iteration i>1 carries the gate condition !(P@(i-1)) — a settled loop skips
// all later iterations through ordinary condition evaluation, no scheduler
// special-casing. After the chain, a selector node per referenced body step
// coalesces X@N..X@1 so post-loop references read the final executed
// iteration; the loop-exhaustion failure rule rides on the selector.
func (d *desugarer) unrollLoop(b *builder, wf WorkflowDef, s StepDef, lvl *level, sp string, gates []condPiece) {
	ld, ok := lvl.loops[s.ID]
	if !ok || ld.def.Max < 1 || ld.def.Until == "" || len(ld.def.Steps) == 0 {
		d.addf(sp+".loop", "loop requires max >= 1, until, and a non-empty body")
		return
	}
	projected := countSteps(ld.def.Steps) * ld.def.Max
	if d.nodeCount+projected > maxUnrolledNodes {
		d.addf(sp+".loop", "unrolling loop %q would exceed the %d-node sanity ceiling (%d body nodes x max %d)",
			s.ID, maxUnrolledNodes, countSteps(ld.def.Steps), ld.def.Max)
		return
	}

	// The loop-step's own when: applies to every iteration's nodes.
	iterGates := gates
	if s.When != "" {
		if piece, ok := d.condPieceFor(b, s.When, lvl, sp+".when"); ok {
			iterGates = append(append([]condPiece(nil), gates...), piece)
		}
	}

	untilAt := make([]string, ld.def.Max+1)
	untilDepsAt := make([][]string, ld.def.Max+1)
	var prevNodes []string
	for i := 1; i <= ld.def.Max; i++ {
		instPrefix := ld.instancePrefix(i)
		ilvl := newLevel(lvl, instPrefix, ld.def.Steps)

		// until: is evaluated against iteration i's own instances and may only
		// reference body steps.
		u, udeps, err := rewriteStepRefs(ld.def.Until, d.untilResolver(ld, i))
		if err != nil {
			d.addf(sp+".loop.until", "%v", err)
			return
		}
		untilAt[i], untilDepsAt[i] = u, udeps

		g := iterGates
		if i > 1 {
			gate := condPiece{cel: "!(" + untilAt[i-1] + ")", deps: untilDepsAt[i-1], wrapped: true}
			g = append([]condPiece{gate}, iterGates...)
		}

		start := len(b.nodes)
		d.emitSteps(b, wf, ld.def.Steps, ilvl, sp+".loop.steps", g)

		// The linear chain: every node of iteration i-1 orders every node of
		// iteration i. Only nodes under this instance's prefix participate —
		// lazily created outer selectors do not join the chain.
		var curNodes []string
		for _, n := range b.nodes[start:] {
			if strings.HasPrefix(n.ID, instPrefix+"/") {
				curNodes = append(curNodes, n.ID)
			}
		}
		sort.Strings(curNodes)
		for _, prev := range prevNodes {
			for _, cur := range curNodes {
				b.edges = append(b.edges, Edge{From: prev, To: cur})
			}
		}
		prevNodes = curNodes
	}

	// A selector exists for every body step the until predicate reads, in
	// addition to any created on demand by outside references.
	_, _, err := rewriteStepRefs(ld.def.Until, func(step string) (string, error) {
		if !ld.body[step] {
			return "", fmt.Errorf("until references step %q outside the loop body", step)
		}
		if _, err := d.ensureSelector(b, ld, step, sp); err != nil {
			return "", err
		}
		return step, nil
	})
	if err != nil {
		d.addf(sp+".loop.until", "%v", err)
	}
}

// untilResolver rewrites until: refs to iteration i's instances, rejecting
// references to steps outside the loop body.
func (d *desugarer) untilResolver(ld *loopDecl, i int) func(string) (string, error) {
	return func(step string) (string, error) {
		if !ld.body[step] {
			return "", fmt.Errorf("until references step %q outside the loop body", step)
		}
		return ld.instancePrefix(i) + "/" + step, nil
	}
}

// ensureSelector creates (once) the selector node for loop body step X: an
// alias whose candidates are X@Max..X@1, newest first, plus the
// loop-exhaustion rule evaluated against the final iteration.
func (d *desugarer) ensureSelector(b *builder, ld *loopDecl, step, sp string) (string, error) {
	selID := ld.prefix + "/" + step
	if b.made[selID] {
		return selID, nil
	}
	candidates := make([]string, 0, ld.def.Max)
	for i := ld.def.Max; i >= 1; i-- {
		candidates = append(candidates, ld.instancePrefix(i)+"/"+step)
	}
	u, udeps, err := rewriteStepRefs(ld.def.Until, d.untilResolver(ld, ld.def.Max))
	if err != nil {
		return "", err
	}
	b.addNode(Node{
		ID:   selID,
		Kind: KindSelector,
		Sel: &SelSpec{
			Step:       step,
			Candidates: candidates,
			Exhausted:  &CondSpec{CEL: "!(" + u + ")", Deps: udeps},
		},
	})
	d.nodeCount++
	for _, c := range candidates {
		b.edges = append(b.edges, Edge{From: c, To: selID})
	}
	return selID, nil
}

// expandBindings turns each with: entry into either a data edge (${steps...}),
// a param/item binding descriptor, or a typed literal.
func (d *desugarer) expandBindings(b *builder, with map[string]any, lvl *level, path, toNode string, allowItem bool) map[string]BindingDesc {
	out := map[string]BindingDesc{}
	for _, slot := range sortedKeys(with) {
		val := with[slot]
		slotPath := path + "." + slot
		bind, err := ParseBinding(val)
		if err != nil {
			d.addf(slotPath, "%v", err)
			continue
		}
		if !bind.IsRef {
			nv, err := normalizeValue(val)
			if err != nil {
				d.addf(slotPath, "invalid literal: %v", err)
				continue
			}
			out[slot] = BindingDesc{Kind: BindLiteral, Value: nv, Type: yamlTypeName(nv)}
			continue
		}
		switch bind.Ref.Root {
		case RootParams:
			out[slot] = BindingDesc{Kind: BindParam, Name: bind.Ref.Name}
		case RootItem:
			if !allowItem {
				d.addf(slotPath, "${item.*} is only legal inside a generate's with: bindings")
				continue
			}
			out[slot] = BindingDesc{Kind: BindItem, Field: bind.Ref.Name}
		case RootSteps:
			from, err := d.resolveStep(b, lvl, bind.Ref.Name)
			if err != nil {
				d.addf(slotPath, "%v", err)
				continue
			}
			b.edges = append(b.edges, Edge{From: from, FromPort: bind.Ref.Field, To: toNode, ToPort: slot})
		case RootSources:
			d.addf(slotPath, "${sources.*} is not a legal binding value")
		}
	}
	return out
}

// resolveStep maps a step name to the IR node instance the current scope sees:
// inside a loop body iteration it is the same-iteration instance, outside a
// loop it is the loop's selector (created on demand — this is exactly the
// "referenced outside the loop" rule), otherwise the plain node.
func (d *desugarer) resolveStep(b *builder, lvl *level, step string) (string, error) {
	for l := lvl; l != nil; l = l.parent {
		if id, ok := l.plain[step]; ok {
			return id, nil
		}
		if _, ok := l.loops[step]; ok {
			return "", fmt.Errorf("steps.%s references a loop step, which has no output fields; reference a body step instead", step)
		}
		for _, loopID := range sortedKeys(l.loops) {
			ld := l.loops[loopID]
			if ld.body[step] {
				return d.ensureSelector(b, ld, step, "")
			}
		}
	}
	return "", fmt.Errorf("unknown step %q", step)
}

// stepCond builds a node's condition from its own when: plus imposed gates.
func (d *desugarer) stepCond(b *builder, when string, lvl *level, sp string, gates []condPiece) *CondSpec {
	pieces := append([]condPiece(nil), gates...)
	if when != "" {
		if piece, ok := d.condPieceFor(b, when, lvl, sp+".when"); ok {
			pieces = append(pieces, piece)
		}
	}
	return combineCond(pieces)
}

func (d *desugarer) condPieceFor(b *builder, when string, lvl *level, path string) (condPiece, bool) {
	cel, deps, err := rewriteStepRefs(when, func(step string) (string, error) {
		return d.resolveStep(b, lvl, step)
	})
	if err != nil {
		d.addf(path, "%v", err)
		return condPiece{}, false
	}
	return condPiece{cel: cel, deps: deps}, true
}

// emitDependsOn expands depends_on into pure ordering edges. Depending on a
// loop step orders after every final-iteration body node.
func (d *desugarer) emitDependsOn(b *builder, s StepDef, lvl *level, sp, nodeID string) {
	for _, dep := range s.DependsOn {
		froms, err := d.resolveDependsOn(lvl, dep)
		if err != nil {
			d.addf(sp+".depends_on", "%v", err)
			continue
		}
		for _, from := range froms {
			b.edges = append(b.edges, Edge{From: from, To: nodeID})
		}
	}
}

func (d *desugarer) resolveDependsOn(lvl *level, dep string) ([]string, error) {
	for l := lvl; l != nil; l = l.parent {
		if id, ok := l.plain[dep]; ok {
			return []string{id}, nil
		}
		if ld, ok := l.loops[dep]; ok {
			var out []string
			for _, bodyStep := range sortedKeys(ld.body) {
				out = append(out, ld.instancePrefix(ld.def.Max)+"/"+bodyStep)
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("unknown step %q in depends_on", dep)
}

// resolveTemplate collapses a dual-mode TemplateDef into the ResolvedTemplate
// the executor consumes; both the named and inline forms produce the same value.
// The Loader has already proven every reference resolves and exclusivity holds,
// so this only rearranges resolved values — it reads no files (paths are already
// absolute from assembly).
func (d *desugarer) resolveTemplate(name, sp string) *ResolvedTemplate {
	t, ok := d.cfg.Templates[name]
	if !ok {
		d.addf(sp+".use", "unknown template %q", name)
		return nil
	}
	build, _ := ResolveBuild(d.cfg, t)
	rt := &ResolvedTemplate{
		Name:      name,
		Packages:  append([]string(nil), build.Packages...),
		Overlay:   build.Overlay,
		Pin:       build.Pin, // flat *PinDef, json "pin,omitempty"; nil ⇒ omitted ⇒ byte-identical IR
		Identity:  resolveIdentity(t),
		Resources: t.Run.Resources,
		Runtime:   t.Run.Runtime,
		Env:       t.Run.Env,
		Volumes:   t.Run.Volumes,
		Skill:     t.Skill,
		Hooks:     resolveHooks(d.cfg, t.Hooks),
		Skills:    resolveSkills(d.cfg, t),
		Inputs:    d.normalizeDefs(t.Inputs, sp),
		Output:    d.normalizeDefs(t.Output, sp),
	}
	return rt
}

// ResolveBuild returns the effective toolset for a template: its inline build:
// if set, else the referenced image: from the Images library. The bool is false
// only when neither is set (a validation error the Loader already reported). It
// lets the desugarer and infra's build seam share one resolution.
func ResolveBuild(cfg *Config, t TemplateDef) (BuildDef, bool) {
	if t.Build != nil {
		return *t.Build, true
	}
	if t.Image != "" {
		if img, ok := cfg.Images[t.Image]; ok {
			// ImageDef and BuildDef are distinct types, so the named form must
			// COPY img.Pin → build.Pin explicitly; omit it and a named image:'s
			// pin is silently dropped before it reaches the IR or infra.
			return BuildDef{Packages: img.Packages, Overlay: img.Overlay, Pin: img.Pin}, true
		}
	}
	return BuildDef{}, false
}

// resolveIdentity picks the identity name from the top-level alias or run.identity.
func resolveIdentity(t TemplateDef) string {
	if t.Identity != "" {
		return t.Identity
	}
	return t.Run.Identity
}

// resolveHooks resolves each hooks.<field>: a path form passes through verbatim
// (already absolute from assembly), a bare name resolves to its Hooks-library
// path. Both yield the resolved hook path the IR has always carried.
func resolveHooks(cfg *Config, h HookSet) HookSet {
	return HookSet{
		Context:   resolveHook(cfg, h.Context),
		Prelude:   resolveHook(cfg, h.Prelude),
		OnFailure: resolveHook(cfg, h.OnFailure),
	}
}

func resolveHook(cfg *Config, v string) string {
	if v == "" || isPath(v) {
		return v
	}
	if hd, ok := cfg.Hooks[v]; ok {
		return hd.Path
	}
	return v // dangling bare name — the Loader already reported it
}

// resolveSkills projects the unmarshal-time SkillsRef into the resolved delivery
// set. Named skills become an ordered, name-deduped Sources set (the run-prep
// stager farms them under <name>/); an inline {dir, link} becomes Root (a
// skills-root the stager mounts directly, no <name> wrapper); absent yields no
// skills leg.
func resolveSkills(cfg *Config, t TemplateDef) *ResolvedSkills {
	switch {
	case len(t.Skills.Names) > 0:
		seen := map[string]bool{}
		var srcs []SkillSource
		for _, nm := range t.Skills.Names {
			if seen[nm] {
				continue
			}
			seen[nm] = true
			srcs = append(srcs, SkillSource{Name: nm, Dir: cfg.Skills[nm].Dir})
		}
		return &ResolvedSkills{Sources: srcs, Primary: t.Skill, Link: t.SkillsLink}
	case t.Skills.Inline != nil:
		return &ResolvedSkills{Root: t.Skills.Inline.Dir, Primary: t.Skill, Link: t.Skills.Inline.Link}
	default:
		return nil
	}
}

func (d *desugarer) normalizeDefs(defs map[string]ParamDef, sp string) map[string]ParamDef {
	out := make(map[string]ParamDef, len(defs))
	for _, k := range sortedKeys(defs) {
		def := defs[k]
		if def.Default != nil {
			nv, err := normalizeValue(def.Default)
			if err != nil {
				d.addf(sp, "invalid default for %q: %v", k, err)
			} else {
				def.Default = nv
			}
		}
		out[k] = def
	}
	return out
}

// countSteps counts the IR nodes a step list will emit (loops multiplied by
// max), used by the unroll sanity ceiling.
func countSteps(steps []StepDef) int {
	n := 0
	for _, s := range steps {
		if s.Loop != nil {
			n += countSteps(s.Loop.Steps) * s.Loop.Max
			continue
		}
		n++
	}
	return n
}

// checkWorkflowRefCycles rejects workflow reuse cycles reachable from entry:
// unbounded structures cannot unroll. Generate references are excluded — a
// generate keeps its target by name and expands at run time.
func checkWorkflowRefCycles(cfg *Config, entry string) error {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	state := map[string]int{}
	var stack []string
	var visit func(wf string) error
	visit = func(wf string) error {
		state[wf] = grey
		stack = append(stack, wf)
		var walk func(steps []StepDef) error
		walk = func(steps []StepDef) error {
			for _, s := range steps {
				if s.Loop != nil {
					if err := walk(s.Loop.Steps); err != nil {
						return err
					}
					continue
				}
				if s.Use == "" {
					continue
				}
				if _, ok := cfg.Workflows[s.Use]; !ok {
					continue
				}
				switch state[s.Use] {
				case grey:
					cycle := append(stack[indexOf(stack, s.Use):], s.Use)
					return &FieldError{
						Path: "workflows." + entry,
						Msg:  "workflow reference cycle: " + strings.Join(cycle, " -> "),
					}
				case white:
					if err := visit(s.Use); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := walk(cfg.Workflows[wf].Steps); err != nil {
			return err
		}
		stack = stack[:len(stack)-1]
		state[wf] = black
		return nil
	}
	return visit(entry)
}

func indexOf(s []string, v string) int {
	for i, e := range s {
		if e == v {
			return i
		}
	}
	return 0
}
