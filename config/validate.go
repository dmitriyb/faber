package config

import (
	"errors"
	"fmt"
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

// Validate runs every schema-level check that can be phrased against the YAML
// alone: structural rules, name discipline, name-level cross-references, and
// binding syntax. All violations are collected — never fatal-first — and
// joined, so a broken file is fixed in one round trip. Checks that need the
// desugared graph (reference resolution, cycles, type flow) belong to
// CheckWiring.
func Validate(cfg *Config) error {
	v := &validator{cfg: cfg}
	v.checkVersion()
	v.checkNetwork()
	v.checkRemote()
	v.checkCredentials()
	v.checkNameDiscipline()
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
	for _, name := range sortedKeys(v.cfg.Credentials.Services) {
		svc := v.cfg.Credentials.Services[name]
		if !serviceNamePattern.MatchString(name) {
			v.addf("credentials.services."+name, "invalid service name (must match %s)", serviceNamePattern)
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
	}
	for _, section := range sections {
		for _, k := range section.keys {
			if k == "" {
				v.addf(section.name, "empty name")
			}
		}
	}
	for _, name := range sortedKeys(v.cfg.Templates) {
		if _, dup := v.cfg.Workflows[name]; dup {
			v.addf("templates."+name, "name collides with a workflow of the same name; use: would be ambiguous")
		}
	}
}

func (v *validator) checkTemplates() {
	for _, name := range sortedKeys(v.cfg.Templates) {
		t := v.cfg.Templates[name]
		path := "templates." + name
		if t.Skill == "" {
			v.addf(path+".skill", "required")
		}
		if len(t.Build.Packages) == 0 {
			v.addf(path+".build.packages", "must be non-empty (the toolset is the environment)")
		}
		if t.Run.Identity != "" {
			if _, ok := v.cfg.Identities[t.Run.Identity]; !ok {
				v.addf(path+".run.identity", "unknown identity %q", t.Run.Identity)
			}
		}
		if m := t.Run.Resources.Memory; m != "" && !memoryPattern.MatchString(m) {
			v.addf(path+".run.resources.memory", "invalid memory string %q", m)
		}
		if t.Run.Resources.CPUs < 0 {
			v.addf(path+".run.resources.cpus", "must be >= 0")
		}
		v.checkParamDefs(path+".inputs", t.Inputs)
		v.checkParamDefs(path+".output", t.Output)
	}
}

func (v *validator) checkParamDefs(path string, defs map[string]ParamDef) {
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
