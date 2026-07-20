package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// FieldError is one schema-level violation with a YAML field path, e.g.
// "workflows.epic.steps[0].with.repo: <reason>". The field path is the
// contract, asserted by tests.
type FieldError struct {
	Path string
	Msg  string
}

func (e *FieldError) Error() string { return e.Path + ": " + e.Msg }

// serviceModes is the closed set of credential handle modes.
var serviceModes = map[string]bool{"proxy": true, "file": true, "helper": true}

// serviceNamePattern is the closed charset for credential service names: they
// are embedded in env-var names, mount specs, and /run/secrets paths, so
// anything outside it (':', '=', '/', spaces) must fail at load, not as an
// opaque docker error mid-run.
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var memoryPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[bkmgBKMG]?$`)

// stepIDPattern is the closed grammar for step ids. Node ids are built from
// them with the reserved namespacing characters ('/' for scope, '@' for loop
// iterations, '[' ']' for generate instances, '"' for CEL keys), so a step id
// containing one could collide with a desugared id or break the canonical
// steps["<id>"] form — caught here, not as a duplicate-node abort at run
// start. The grammar is also reference-total: a dot would make the id
// unreferenceable in ${steps...} bindings (the ref grammar splits on dots)
// and a digit-first id unreferenceable in when: (CEL identifiers are
// letter-first), so both fail at declaration, not at the referencing site.
var stepIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// slotNamePattern is the closed grammar for input-slot and param names: they
// embed into env-variable names (FABER_INPUT_<TOKEN>) and CEL references, so
// the charset is the env-safe one and the first character is a letter.
var slotNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// EnvToken is the canonical slot/param/service name → env-token mapping
// (uppercase, '-'→'_'). It is the single source both halves of the box env
// contract use: the agent contract's SlotToken delegates here, so validate-
// time collision checks and run-time export can never disagree.
func EnvToken(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// RunnerOwnedEnv are the process-runner variables (beyond the engine-owned
// names) that user-named material — credential services, bundle sidecars —
// must never shadow inside the box.
var RunnerOwnedEnv = map[string]bool{"PATH": true, "HOME": true, "GIT_SSH_COMMAND": true, "SSH_AUTH_SOCK": true}

// EngineOwnedEnv reports whether an environment name belongs to the engine
// or security env contract: the whole FABER_ namespace plus the forwarded
// agent socket. The single source for validate-time checks and the box
// contract's run-time gate (agent/contract delegates here).
func EngineOwnedEnv(key string) bool {
	return strings.HasPrefix(key, "FABER_") || key == "SSH_AUTH_SOCK"
}

// ReservedContainerPaths are the container paths the box run contract and
// the security bindings own; template volumes may not touch them. The
// run-spec assembler consumes this same list (a cross-package test asserts
// it covers the security module's mount constants).
var ReservedContainerPaths = []string{"/faber", "/run/secrets", "/ssh-agent", "/workspace", "/home/box", "/tmp"}

// pinCharset restricts a per-toolset nixpkgs pin's rev/sha256 to a splice-safe
// charset. Because a pin may now be user-supplied YAML that is spliced into
// infra's rendered fetchTarball call, its fields are validated here at the
// Loader — config owns the authoritative check; the infra splice keeps a
// matching guard purely as defense-in-depth. Config never imports infra, so the
// charset lives on both sides independently but must stay in sync.
var pinCharset = regexp.MustCompile(`^[A-Za-z0-9:+/=._-]+$`)

// hasDotDotSegment reports whether p contains a ".." path segment (either
// slash separator), i.e. a component that would climb out of its base dir.
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// safeName reports whether name is usable as a single filesystem path segment.
// Library keys and the referenced names that faber joins into a host path — most
// sharply a skill name, staged under <stage>/<name>, but the same principle for
// every image/hook/template/workflow/identity identifier — must not carry a
// separator, a ".." segment, an absolute root, or a leading "." / "~", any of
// which would let the joined path escape the dir it is anchored to. This is the
// same discipline serviceNamePattern enforces on credential service names,
// applied to every identifier that becomes a path component. A violation is a
// validate-time error, never a mid-run surprise inside copyTree.
func safeName(name string) bool {
	if name == "" || filepath.IsAbs(name) {
		return false
	}
	if name[0] == '.' || name[0] == '~' {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return !hasDotDotSegment(name)
}

// Validate runs every schema-level check that can be phrased against the
// assembled YAML alone: structural rules, name discipline, dual-mode
// exclusivity, name-level library cross-references, and binding syntax. It also
// folds in the AssemblyViolations Assemble recorded (duplicate library keys,
// substrate on a non-root file) — the two cross-file rules the merged Config can
// no longer express — so the user sees assembly and schema errors together in
// one round trip. All violations are collected, never fatal-first, and joined.
// Checks that need the desugared graph (typed reference flow, cycles) belong to
// CheckWiring.
func Validate(cfg *Config, viols []AssemblyViolation) error {
	v := &validator{cfg: cfg}
	for _, av := range viols {
		v.errs = append(v.errs, &FieldError{Path: av.Path, Msg: av.Msg})
	}
	v.checkVersion()
	v.checkNetwork()
	v.checkRemote()
	v.checkCredentials()
	v.checkNameDiscipline()
	v.checkImages()
	v.checkTemplates()
	v.checkWorkflows()
	return errors.Join(v.errs...)
}

type validator struct {
	cfg  *Config
	errs []error
}

func (v *validator) addf(path, format string, args ...any) {
	v.errs = append(v.errs, &FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

func (v *validator) checkVersion() {
	if v.cfg.Version != 1 {
		v.addf("version", "must be 1 (got %d)", v.cfg.Version)
	}
}

// checkNetwork enforces that a configured network section names its network
// (an unnamed section would silently run steps on the default bridge with
// unrestricted egress) and declares exactly one egress mode — proxy or
// nftables — so NET_ADMIN is never granted by omission.
func (v *validator) checkNetwork() {
	n := v.cfg.Network
	if n.Name == "" && n.Proxy == "" && len(n.NoProxy) == 0 && !n.Nftables {
		return // network section absent
	}
	if n.Name == "" {
		v.addf("network.name", "required when a network is configured")
	}
	if n.Proxy != "" && n.Nftables {
		v.addf("network", "exactly one of proxy / nftables must be set (both given)")
	}
	if n.Proxy == "" && !n.Nftables {
		v.addf("network", "exactly one of proxy / nftables must be set (neither given)")
	}
}

func (v *validator) checkRemote() {
	r := v.cfg.Remote
	if r.URL == "" && r.HostKeyFile == "" && !r.TOFU {
		return // remote section absent
	}
	if r.URL == "" {
		v.addf("remote.url", "required when a remote is configured")
	}
	if r.HostKeyFile != "" && r.TOFU {
		v.addf("remote", "exactly one of host_key_file / tofu must be set (both given)")
	}
	if r.HostKeyFile == "" && !r.TOFU {
		v.addf("remote", "exactly one of host_key_file / tofu must be set (neither given)")
	}
}

func (v *validator) checkCredentials() {
	tokens := map[string]string{} // env token -> first service claiming it
	for _, name := range sortedKeys(v.cfg.Credentials.Services) {
		svc := v.cfg.Credentials.Services[name]
		if !serviceNamePattern.MatchString(name) {
			v.addf("credentials.services."+name, "invalid service name (must match %s)", serviceNamePattern)
		} else if tok := EnvToken(name); RunnerOwnedEnv[tok] || EngineOwnedEnv(tok) {
			// The box exports each secret under its uppercased name; a service
			// named "path" or "home" would shadow the runner-owned variable in
			// every hook and agent invocation.
			v.addf("credentials.services."+name, "service name exports the engine- or runner-owned variable %s inside the box; rename the service", tok)
		} else if first, dup := tokens[tok]; dup {
			// The box exports each secret under its env token last-wins; two
			// services sharing a token would silently shadow one another.
			v.addf("credentials.services."+name, "service name collides with %q: both export the env variable %s", first, tok)
		} else {
			tokens[tok] = name
		}
		if !serviceModes[svc.Mode] {
			v.addf("credentials.services."+name+".mode", "unknown mode %q (legal: proxy, file, helper)", svc.Mode)
		}
	}
}

// checkNameDiscipline enforces non-empty names and the template/workflow name
// disjointness that keeps use: unambiguous.
func (v *validator) checkNameDiscipline() {
	sections := []struct {
		name string
		keys []string
	}{
		{"identities", sortedKeys(v.cfg.Identities)},
		{"templates", sortedKeys(v.cfg.Templates)},
		{"workflows", sortedKeys(v.cfg.Workflows)},
		{"images", sortedKeys(v.cfg.Images)},
		{"skills", sortedKeys(v.cfg.Skills)},
		{"hooks", sortedKeys(v.cfg.Hooks)},
	}
	for _, section := range sections {
		for _, k := range section.keys {
			switch {
			case k == "":
				v.addf(section.name, "empty name")
			case !safeName(k):
				// The key is joined into a host path (a skills stage dir, a
				// per-attempt tree), so it must be a safe single segment.
				v.addf(fmt.Sprintf("%s.%q", section.name, k),
					`name must be a safe identifier (no "/", "..", or leading ".")`)
			}
		}
	}
	for _, name := range sortedKeys(v.cfg.Templates) {
		if _, dup := v.cfg.Workflows[name]; dup {
			v.addf("templates."+name, "name collides with a workflow of the same name; use: would be ambiguous")
		}
	}
}

// checkImages validates each library image's optional nixpkgs pin. A
// fully-empty pin: {} was already normalized to nil at load, so a non-nil pin
// here has at least one field set and must be complete and charset-clean.
func (v *validator) checkImages() {
	for _, name := range sortedKeys(v.cfg.Images) {
		v.checkPin("images."+name+".pin", v.cfg.Images[name].Pin)
	}
}

// checkPin enforces the per-toolset pin's completeness and charset, field-pathed
// and collected. A nil pin is absent (legal — it selects faber's default). A
// present pin needs both rev and sha256 (a partial pin is the completeness
// error at path), and each present value must match the splice-safe charset
// (an off-charset value is its own field-pathed error at path.rev / path.sha256).
func (v *validator) checkPin(path string, pin *PinDef) {
	if pin == nil {
		return
	}
	if pin.Rev == "" || pin.SHA256 == "" {
		v.addf(path, "rev and sha256 are both required")
	}
	if pin.Rev != "" && !pinCharset.MatchString(pin.Rev) {
		v.addf(path+".rev", "invalid characters (allowed: %s)", pinCharset)
	}
	if pin.SHA256 != "" && !pinCharset.MatchString(pin.SHA256) {
		v.addf(path+".sha256", "invalid characters (allowed: %s)", pinCharset)
	}
}

func (v *validator) checkTemplates() {
	for _, name := range sortedKeys(v.cfg.Templates) {
		t := v.cfg.Templates[name]
		path := "templates." + name
		if t.Skill == "" {
			v.addf(path+".skill", "required")
		}
		v.checkToolset(path, t)
		if t.Build != nil {
			v.checkPin(path+".build.pin", t.Build.Pin)
		}
		v.checkIdentity(path, t)
		v.checkHooks(path, t)
		v.checkSkills(path, t)
		if m := t.Run.Resources.Memory; m != "" && !memoryPattern.MatchString(m) {
			v.addf(path+".run.resources.memory", "invalid memory string %q", m)
		}
		if t.Run.Resources.CPUs < 0 {
			v.addf(path+".run.resources.cpus", "must be >= 0")
		}
		v.checkParamDefs(path+".inputs", t.Inputs)
		v.checkParamDefs(path+".output", t.Output)
		v.checkRunContract(path, t)
	}
}

// checkRunContract mirrors the run-spec assembler's statically checkable
// refusals (agent.BuildRunSpec) at validate altitude, so a template that
// cannot launch any box fails `faber validate`, not each step at run time:
// the agent CLI must be named, template env may not claim engine- or
// security-owned variables, and template volumes may not overlap the
// reserved container paths.
func (v *validator) checkRunContract(path string, t TemplateDef) {
	if t.Run.Env["FABER_AGENT_CLI"] == "" {
		v.addf(path+".run.env.FABER_AGENT_CLI", "required (faber defaults no agent vendor); name the agent CLI binary the box invokes")
	}
	for _, key := range sortedKeys(t.Run.Env) {
		if key == "FABER_AGENT_CLI" {
			continue // the one documented template-env exception
		}
		if EngineOwnedEnv(key) {
			v.addf(path+".run.env."+key, "engine- or security-owned name (only FABER_AGENT_CLI may be set here)")
		}
	}
	for _, host := range sortedKeys(t.Run.Volumes) {
		container := t.Run.Volumes[host]
		for _, r := range ReservedContainerPaths {
			if containerPathUnder(container, r) || containerPathUnder(r, container) {
				v.addf(path+".run.volumes."+host, "container path %q overlaps the reserved path %q", container, r)
				break
			}
		}
	}
}

// containerPathUnder reports whether cleaned path a equals b or lies beneath
// it (the run-spec assembler's overlap rule).
func containerPathUnder(a, b string) bool {
	a, b = filepath.ToSlash(filepath.Clean(a)), filepath.ToSlash(filepath.Clean(b))
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/")
}

// checkToolset enforces the image/build dual-mode: exactly one form, the named
// image must exist in the Images library, and the effective package list must
// be non-empty (the toolset is the environment).
func (v *validator) checkToolset(path string, t TemplateDef) {
	hasImage, hasBuild := t.Image != "", t.Build != nil
	switch {
	case hasImage && hasBuild:
		v.addf(path, "image and build are mutually exclusive")
	case !hasImage && !hasBuild:
		v.addf(path, "a toolset is required: set image (a name) or build (inline)")
	case hasBuild:
		if len(t.Build.Packages) == 0 {
			v.addf(path+".build.packages", "must be non-empty (the toolset is the environment)")
		}
	case hasImage:
		img, ok := v.cfg.Images[t.Image]
		if !ok {
			v.addf(path+".image", "unknown image %q%s", t.Image, didYouMean(t.Image, sortedKeys(v.cfg.Images)))
		} else if len(img.Packages) == 0 {
			v.addf(path+".image", "image %q has no packages (the toolset is the environment)", t.Image)
		}
	}
}

// checkIdentity enforces the identity dual-mode: top-level identity: and
// run.identity: are mutually exclusive aliases, and the chosen one must name a
// declared identity.
func (v *validator) checkIdentity(path string, t TemplateDef) {
	if t.Identity != "" && t.Run.Identity != "" {
		v.addf(path, "identity and run.identity are mutually exclusive")
	}
	ident, identPath := t.Run.Identity, path+".run.identity"
	if t.Identity != "" {
		ident, identPath = t.Identity, path+".identity"
	}
	if ident != "" {
		if _, ok := v.cfg.Identities[ident]; !ok {
			v.addf(identPath, "unknown identity %q%s", ident, didYouMean(ident, sortedKeys(v.cfg.Identities)))
		}
	}
}

// checkHooks resolves each hooks.<field>: a bare name (not a path) must name a
// declared hook; a dangling bare name is a reference error, never a silent path.
// Path forms are opaque and never checked for existence.
func (v *validator) checkHooks(path string, t TemplateDef) {
	fields := []struct{ name, value string }{
		{"context", t.Hooks.Context},
		{"prelude", t.Hooks.Prelude},
		{"on_failure", t.Hooks.OnFailure},
	}
	for _, f := range fields {
		if f.value == "" || isPath(f.value) {
			continue
		}
		if _, ok := v.cfg.Hooks[f.value]; !ok {
			v.addf(path+".hooks."+f.name, "unknown hook %q%s", f.value, didYouMean(f.value, sortedKeys(v.cfg.Hooks)))
		}
	}
}

// checkSkills enforces the skills dual-mode. Named mode (skills: [names]):
// skills_link is required, every name and the primary skill resolve against the
// Skills library, and the primary skill must be one of the delivered names.
// Inline mode (skills: {dir, link}): the all-or-nothing dir/link pair, and the
// primary skill is a free-form prompt token (no membership check). Absent: no
// skills leg — and skills_link with no leg to deliver is a violation.
func (v *validator) checkSkills(path string, t TemplateDef) {
	named := len(t.Skills.Names) > 0
	inline := t.Skills.Inline != nil

	switch {
	case named:
		if t.SkillsLink == "" {
			v.addf(path+".skills_link", "required when skills is a named list")
		} else {
			v.checkLink(path+".skills_link", t.SkillsLink)
		}
		for i, nm := range t.Skills.Names {
			skPath := fmt.Sprintf("%s.skills[%d]", path, i)
			// A referenced name is staged under <stage>/<name>; an unsafe name
			// would let staging escape the per-attempt tree, so reject it before
			// (and instead of) the membership check to avoid a noisy second error.
			if !safeName(nm) {
				v.addf(skPath, `skill name must be a safe identifier (no "/", "..", or leading "."): %q`, nm)
				continue
			}
			if _, ok := v.cfg.Skills[nm]; !ok {
				v.addf(skPath, "unknown skill %q%s", nm, didYouMean(nm, sortedKeys(v.cfg.Skills)))
			}
		}
		if t.Skill != "" && !contains(t.Skills.Names, t.Skill) {
			v.addf(path+".skill", "primary skill %q must be one of the delivered skills [%s]", t.Skill, strings.Join(t.Skills.Names, ", "))
		}
	case inline:
		if t.SkillsLink != "" {
			v.addf(path+".skills_link", "must not be set with an inline skills mapping (the link lives at skills.link)")
		}
		if t.Skills.Inline.Dir == "" {
			v.addf(path+".skills.dir", "required when skills is present")
		}
		if t.Skills.Inline.Link == "" {
			v.addf(path+".skills.link", "required when skills is present")
		} else {
			v.checkLink(path+".skills.link", t.Skills.Inline.Link)
		}
	default:
		// No skills leg: a discovery path with nothing to deliver is a violation.
		if t.SkillsLink != "" {
			v.addf(path+".skills_link", "set without a skills leg (a discovery path with nothing to deliver)")
		}
	}
}

// checkLink verifies an in-box $HOME-relative discovery path stays under $HOME:
// it is resolved as $HOME/<link> in the box, so an absolute path or a ".."
// segment that would escape is rejected. Contents are otherwise opaque.
func (v *validator) checkLink(fieldPath, link string) {
	switch {
	case filepath.IsAbs(link):
		v.addf(fieldPath, "must be relative to $HOME, not absolute: %q", link)
	case hasDotDotSegment(link):
		v.addf(fieldPath, "must not contain a %q path segment: %q", "..", link)
	}
}

func (v *validator) checkParamDefs(path string, defs map[string]ParamDef) {
	// Name grammar and env-token disjointness: slot and param names embed
	// into FABER_INPUT_<TOKEN> variables, and the token mapping (upper-case,
	// '-'→'_') is lossy — two declared names sharing a token would silently
	// misbind one value to the other's variable.
	tokens := map[string]string{} // token -> first name claiming it
	for _, name := range sortedKeys(defs) {
		if !slotNamePattern.MatchString(name) {
			v.addf(path+"."+name, "invalid name %q (must match %s; names embed into env variables)", name, slotNamePattern)
			continue
		}
		tok := EnvToken(name)
		if first, dup := tokens[tok]; dup {
			v.addf(path+"."+name, "name collides with %q: both map to the env token %s", first, tok)
		} else {
			tokens[tok] = name
		}
	}
	for _, name := range sortedKeys(defs) {
		d := defs[name]
		switch d.Type {
		case "string", "int", "bool", "object":
		case "":
			v.addf(path+"."+name+".type", "required (one of string, int, bool, object)")
		default:
			v.addf(path+"."+name+".type", "unknown type %q (one of string, int, bool, object)", d.Type)
		}
		if len(d.Enum) > 0 && d.Type != "string" {
			v.addf(path+"."+name+".enum", "enum is only valid on string fields")
		}
		if d.Default != nil {
			if got := yamlTypeName(d.Default); got != d.Type {
				v.addf(path+"."+name+".default", "default is %s, declared type is %s", got, d.Type)
			} else if len(d.Enum) > 0 {
				if s, ok := d.Default.(string); !ok || !contains(d.Enum, s) {
					v.addf(path+"."+name+".default", "default %v not in enum [%s]", d.Default, strings.Join(d.Enum, ", "))
				}
			}
		}
	}
}

func (v *validator) checkWorkflows() {
	for _, name := range sortedKeys(v.cfg.Workflows) {
		wf := v.cfg.Workflows[name]
		path := "workflows." + name
		v.checkParamDefs(path+".params", wf.Params)
		for _, src := range sortedKeys(wf.Sources) {
			s := wf.Sources[src]
			if s.Command == "" {
				v.addf(path+".sources."+src+".command", "required")
			}
			for i, arg := range s.Args {
				b, err := ParseBinding(arg)
				argPath := fmt.Sprintf("%s.sources.%s.args[%d]", path, src, i)
				if err != nil {
					v.addf(argPath, "%v", err)
					continue
				}
				if b.IsRef && b.Ref.Root != RootParams {
					v.addf(argPath, "source args may only reference ${params.*} (got %s)", b.Ref)
				}
			}
		}
		if len(wf.Steps) == 0 {
			v.addf(path+".steps", "must be non-empty")
			continue
		}
		seen := map[string]string{} // step id -> path of first occurrence
		v.checkSteps(path+".steps", wf, wf.Steps, seen, stepIDsAt(wf.Steps, nil))
	}
}

// stepIDsAt returns the set of step ids visible for depends_on at one nesting
// level: the ids at this level plus everything visible in enclosing levels.
func stepIDsAt(steps []StepDef, enclosing map[string]bool) map[string]bool {
	ids := map[string]bool{}
	for k := range enclosing {
		ids[k] = true
	}
	for _, s := range steps {
		if s.ID != "" {
			ids[s.ID] = true
		}
	}
	return ids
}

func (v *validator) checkSteps(path string, wf WorkflowDef, steps []StepDef, seen map[string]string, scopeIDs map[string]bool) {
	for i, s := range steps {
		sp := fmt.Sprintf("%s[%d]", path, i)
		if s.ID == "" {
			v.addf(sp+".id", "required")
		} else if !stepIDPattern.MatchString(s.ID) {
			v.addf(sp+".id", "invalid step id %q (must match %s; '/', '@', '[', ']' and '\"' are reserved for node-id namespacing)", s.ID, stepIDPattern)
		} else if first, dup := seen[s.ID]; dup {
			v.addf(sp+".id", "duplicate step id %q (first declared at %s)", s.ID, first)
		} else {
			seen[s.ID] = sp
		}
		if s.Retry < 0 {
			v.addf(sp+".retry", "must be >= 0")
		}
		for _, dep := range s.DependsOn {
			if !scopeIDs[dep] || dep == s.ID {
				v.addf(sp+".depends_on", "unknown step id %q in this scope", dep)
			}
		}

		// The union rule: exactly one of use / loop / generate. On violation,
		// skip form-specific checks so the step yields exactly one error.
		forms := 0
		for _, set := range []bool{s.Use != "", s.Loop != nil, s.Generate != nil} {
			if set {
				forms++
			}
		}
		if forms != 1 {
			v.addf(sp, "step must have exactly one of use/loop/generate")
			continue
		}

		switch {
		case s.Use != "":
			_, isTemplate := v.cfg.Templates[s.Use]
			_, isWorkflow := v.cfg.Workflows[s.Use]
			if !isTemplate && !isWorkflow {
				v.addf(sp+".use", "unknown template or workflow %q", s.Use)
			}
			v.checkBindings(sp+".with", s.With, false)
		case s.Loop != nil:
			if s.Loop.Max < 1 {
				v.addf(sp+".loop.max", "required and must be >= 1 (bounded loops only)")
			}
			if s.Loop.Until == "" {
				v.addf(sp+".loop.until", "required")
			}
			if len(s.Loop.Steps) == 0 {
				v.addf(sp+".loop.steps", "must be non-empty")
				continue
			}
			if len(s.With) > 0 {
				v.addf(sp+".with", "loop steps take no with: bindings")
			}
			v.checkSteps(sp+".loop.steps", wf, s.Loop.Steps, seen, stepIDsAt(s.Loop.Steps, scopeIDs))
		case s.Generate != nil:
			g := s.Generate
			if _, ok := wf.Sources[g.Source]; !ok {
				v.addf(sp+".generate.source", "unknown source %q", g.Source)
			}
			if _, ok := v.cfg.Workflows[g.Workflow]; !ok {
				v.addf(sp+".generate.workflow", "unknown workflow %q", g.Workflow)
			}
			v.checkBindings(sp+".generate.with", g.With, true)
		}
	}
}

// checkBindings verifies binding syntax: every ${...} parses to one of the
// four legal roots, ${item.*} appears only inside generate bindings, and
// ${sources.*} is never a with: value (a source is named by a generate's
// source: field, not interpolated).
func (v *validator) checkBindings(path string, with map[string]any, inGenerate bool) {
	for _, slot := range sortedKeys(with) {
		b, err := ParseBinding(with[slot])
		slotPath := path + "." + slot
		if err != nil {
			v.addf(slotPath, "%v", err)
			continue
		}
		if !b.IsRef {
			continue
		}
		switch b.Ref.Root {
		case RootItem:
			if !inGenerate {
				v.addf(slotPath, "${item.*} is only legal inside a generate's with: bindings")
			}
		case RootSources:
			v.addf(slotPath, "${sources.*} is not a legal binding value; name the source in a generate's source: field")
		}
	}
}
