package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadRef loads the pristine reference fixture; each caller gets a fresh value
// safe to mutate.
func loadRef(t *testing.T) *Config {
	t.Helper()
	cfg, viols, err := Load("testdata/reference.yaml")
	if err != nil {
		t.Fatalf("load reference: %v", err)
	}
	if err := Validate(cfg, viols); err != nil {
		t.Fatalf("reference must be schema-valid: %v", err)
	}
	return cfg
}

func errCount(err error) int {
	if err == nil {
		return 0
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return len(joined.Unwrap())
	}
	return 1
}

func wantErrContaining(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

// Verifies 23a6be447cc8: the struct tree maps 1:1 to YAML — loading the
// reference yields a Config whose every field survives a marshal/unmarshal
// cycle unchanged, with all four templates and both workflows present.
func TestSchemaRoundTrip(t *testing.T) {
	cfg := loadRef(t)
	if got := len(cfg.Templates); got != 4 {
		t.Fatalf("want 4 templates, got %d", got)
	}
	if got := len(cfg.Workflows); got != 2 {
		t.Fatalf("want 2 workflows, got %d", got)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var again Config
	if err := yaml.Unmarshal(data, &again); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(cfg, &again) {
		t.Fatalf("config did not survive a marshal/unmarshal round trip:\n%s", data)
	}
}

// Verifies 23a6be447cc8: a step with two union forms and a step with none each
// yield exactly one violation naming the step path and the union rule.
func TestSchemaStepUnionEnforcement(t *testing.T) {
	tests := []struct {
		name string
		step StepDef
	}{
		{"both use and loop", StepDef{ID: "s", Use: "box", Loop: &LoopDef{Max: 1, Until: "true", Steps: []StepDef{{ID: "b", Use: "box"}}}}},
		{"neither", StepDef{ID: "s"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.Workflows["flow"] = WorkflowDef{Steps: []StepDef{tt.step}}
			err := Validate(cfg, nil)
			wantErrContaining(t, err, "workflows.flow.steps[0]: step must have exactly one of use/loop/generate")
			if n := errCount(err); n != 1 {
				t.Fatalf("want exactly one violation, got %d: %v", n, err)
			}
		})
	}
}

// minimalConfig is a schema-valid config with one template and one workflow,
// used as the base for loader-check mutations.
func minimalConfig() *Config {
	return &Config{
		Version:    1,
		Identities: map[string]IdentityDef{"worker": {Key: "./keys/worker"}},
		Templates: map[string]TemplateDef{
			"box": {
				Build:  &BuildDef{Packages: []string{"git"}},
				Run:    RunDef{Identity: "worker", Env: map[string]string{"FABER_AGENT_CLI": "agent-cli"}},
				Skill:  "act",
				Model:  "agent-model",
				Effort: "high",
				Inputs: map[string]ParamDef{"input": {Type: "string", Required: true}},
				Output: map[string]FieldDef{"result": {Type: "string", Required: true}},
			},
		},
		Workflows: map[string]WorkflowDef{
			"flow": {
				Params: map[string]ParamDef{"subject": {Type: "string", Required: true}},
				Steps:  []StepDef{{ID: "first", Use: "box", With: map[string]any{"input": "${params.subject}"}}},
			},
		},
	}
}

// Verifies 23a6be447cc8: the Loader check catalog — each invalid config
// produces its expected field-path error and no others.
func TestSchemaLoaderCheckCatalog(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			"duplicate step id inside a loop body",
			func(c *Config) {
				wf := c.Workflows["flow"]
				wf.Steps = append(wf.Steps, StepDef{ID: "cycle", Loop: &LoopDef{Max: 2, Until: "true", Steps: []StepDef{
					{ID: "first", Use: "box", With: map[string]any{"input": "x"}},
				}}})
				c.Workflows["flow"] = wf
			},
			`workflows.flow.steps[1].loop.steps[0].id: duplicate step id "first"`,
		},
		{
			"unknown identity in run.identity",
			func(c *Config) {
				tp := c.Templates["box"]
				tp.Run.Identity = "ghost"
				c.Templates["box"] = tp
			},
			`templates.box.run.identity: unknown identity "ghost"`,
		},
		{
			"missing template model",
			func(c *Config) {
				tp := c.Templates["box"]
				tp.Model = ""
				c.Templates["box"] = tp
			},
			"templates.box.model: required",
		},
		{
			"missing template effort",
			func(c *Config) {
				tp := c.Templates["box"]
				tp.Effort = ""
				c.Templates["box"] = tp
			},
			"templates.box.effort: required",
		},
		{
			"item ref outside a generate binding",
			func(c *Config) {
				c.Workflows["flow"].Steps[0].With["input"] = "${item.id}"
			},
			"workflows.flow.steps[0].with.input: ${item.*} is only legal inside a generate's with: bindings",
		},
		{
			"sources ref used as a with value",
			func(c *Config) {
				c.Workflows["flow"].Steps[0].With["input"] = "${sources.feed}"
			},
			"workflows.flow.steps[0].with.input: ${sources.*} is not a legal binding value",
		},
		{
			"remote with both host_key_file and tofu",
			func(c *Config) {
				c.Remote = RemoteDef{URL: "ssh://git@gateway/srv/git", HostKeyFile: "./keys/host", TOFU: true}
			},
			"remote: exactly one of host_key_file / tofu must be set (both given)",
		},
		{
			"unknown credential mode",
			func(c *Config) {
				c.Credentials = CredentialsDef{Resolver: "./hooks/get-token", Services: map[string]ServiceDef{"svc": {Mode: "inline"}}}
			},
			`credentials.services.svc.mode: unknown mode "inline"`,
		},
		{
			"empty workflow steps",
			func(c *Config) {
				c.Workflows["flow"] = WorkflowDef{Steps: nil}
			},
			"workflows.flow.steps: must be non-empty",
		},
		{
			"template and workflow sharing a name",
			func(c *Config) {
				c.Workflows["box"] = WorkflowDef{Steps: []StepDef{{ID: "s", Use: "box", With: map[string]any{"input": "x"}}}}
			},
			"templates.box: name collides with a workflow",
		},
		{
			// Verifies 6e6d0bb46819: the egress lock cannot fail open — a
			// network section without a name would silently run steps on the
			// default bridge.
			"network section without a name",
			func(c *Config) {
				c.Network = NetworkDef{Proxy: "http://egress:8888"}
			},
			"network.name: required when a network is configured",
		},
		{
			// Verifies 6e6d0bb46819: the two egress modes are mutually
			// exclusive.
			"network with both proxy and nftables",
			func(c *Config) {
				c.Network = NetworkDef{Name: "agents-internal", Proxy: "http://egress:8888", Nftables: true}
			},
			"network: exactly one of proxy / nftables must be set (both given)",
		},
		{
			// Verifies 6e6d0bb46819: the egress mode must be chosen
			// explicitly — NET_ADMIN is never granted by omission.
			"network with neither proxy nor nftables",
			func(c *Config) {
				c.Network = NetworkDef{Name: "agents-internal"}
			},
			"network: exactly one of proxy / nftables must be set (neither given)",
		},
		{
			// Verifies 0c5bc0f678b7: service names feed env-var names, -v
			// mount specs, and /run/secrets paths; a hostile charset must
			// fail at load, not as an opaque docker error mid-run.
			"credential service name outside the closed charset",
			func(c *Config) {
				c.Credentials = CredentialsDef{Resolver: "./hooks/get-token", Services: map[string]ServiceDef{"tok:ro": {Mode: "file"}}}
			},
			"credentials.services.tok:ro: invalid service name",
		},
		{
			// The skills leg is an all-or-nothing pair: a link without a dir
			// fails, field-pathed like the rest.
			"skills present with a missing dir",
			func(c *Config) {
				tp := c.Templates["box"]
				tp.Skills = SkillsRef{Inline: &SkillsDef{Link: ".claude/skills"}}
				c.Templates["box"] = tp
			},
			"templates.box.skills.dir: required when skills is present",
		},
		{
			"skills present with a missing link",
			func(c *Config) {
				tp := c.Templates["box"]
				tp.Skills = SkillsRef{Inline: &SkillsDef{Dir: "./skills"}}
				c.Templates["box"] = tp
			},
			"templates.box.skills.link: required when skills is present",
		},
		{
			// link is resolved as $HOME/<link> in the box, so an absolute link
			// escapes $HOME and is rejected.
			"skills link is absolute",
			func(c *Config) {
				tp := c.Templates["box"]
				tp.Skills = SkillsRef{Inline: &SkillsDef{Dir: "./skills", Link: "/etc/skills"}}
				c.Templates["box"] = tp
			},
			"templates.box.skills.link: must be relative to $HOME, not absolute",
		},
		{
			// A ".." segment climbs out of $HOME and is rejected.
			"skills link escapes with ..",
			func(c *Config) {
				tp := c.Templates["box"]
				tp.Skills = SkillsRef{Inline: &SkillsDef{Dir: "./skills", Link: "../../etc/skills"}}
				c.Templates["box"] = tp
			},
			`templates.box.skills.link: must not contain a ".." path segment`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalConfig()
			tt.mutate(cfg)
			err := Validate(cfg, nil)
			wantErrContaining(t, err, tt.wantErr)
			if n := errCount(err); n != 1 {
				t.Fatalf("want exactly one violation, got %d: %v", n, err)
			}
		})
	}
}

// Verifies 23a6be447cc8: a well-formed skills leg validates and desugars onto
// the ResolvedTemplate verbatim (dir and link carried like the hook/overlay
// paths), so the run phase never re-reads the YAML for it.
func TestSchemaSkillsLegResolved(t *testing.T) {
	cfg := minimalConfig()
	tp := cfg.Templates["box"]
	tp.Skills = SkillsRef{Inline: &SkillsDef{Dir: "./skills", Link: ".claude/skills"}}
	cfg.Templates["box"] = tp
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("well-formed skills must validate: %v", err)
	}
	ir, err := Desugar(cfg, "flow")
	if err != nil {
		t.Fatalf("desugar: %v", err)
	}
	var got *ResolvedSkills
	for _, n := range ir.Nodes {
		if n.Template != nil {
			got = n.Template.Skills
		}
	}
	// The inline {dir, link} form resolves to a direct Root mount (no <name>
	// wrapper, no Sources), byte-identical to today's single-dir delivery.
	if got == nil || got.Root != "./skills" || got.Link != ".claude/skills" || len(got.Sources) != 0 {
		t.Fatalf("resolved skills = %+v, want {Root: ./skills, Link: .claude/skills, Sources: []}", got)
	}
}

// Verifies 8a79b4f5c699: domain behavior enters only through opaque values —
// hook paths, data-source commands, and resolver strings pass through the
// pipeline verbatim and uninterpreted, and the binding grammar rejects any
// root outside the closed set (no environment or host access can be smuggled
// into a binding).
func TestOpaquePolicySeams(t *testing.T) {
	cfg := loadRef(t)
	ir, err := Desugar(cfg, "task")
	if err != nil {
		t.Fatalf("desugar: %v", err)
	}
	var implement *Node
	for i := range ir.Nodes {
		if ir.Nodes[i].ID == "task/implement" {
			implement = &ir.Nodes[i]
		}
	}
	if implement == nil {
		t.Fatal("task/implement node missing")
	}
	hooks := implement.Template.Hooks
	if hooks.Context != "./hooks/gather-context" || hooks.Prelude != "./hooks/claim-item" || hooks.OnFailure != "./hooks/release-item" {
		t.Fatalf("hook paths must pass through verbatim, got %+v", hooks)
	}

	epic, err := Desugar(cfg, "epic")
	if err != nil {
		t.Fatalf("desugar epic: %v", err)
	}
	gen := epic.Nodes[0].Gen
	if gen.Command != "./hooks/list-members" || len(gen.Args) != 1 || gen.Args[0] != "${params.group}" {
		t.Fatalf("data-source command must pass through verbatim, got %+v", gen)
	}

	for _, bad := range []string{"${env.HOME}", "${secrets.token}", "${host.path}"} {
		if _, err := ParseBinding(bad); err == nil {
			t.Fatalf("binding %q must be rejected (closed root set)", bad)
		}
	}
	if _, err := ParseBinding("prefix ${params.x} suffix"); err == nil {
		t.Fatal("string templating around a reference must be rejected")
	}
}

// Verifies 255893ae16eb: params are validated against the declaration before
// anything runs — typing, enum membership, required presence, defaults, and
// unknown-name rejection, all violations joined.
func TestTypedParamsInterface(t *testing.T) {
	decl := map[string]ParamDef{
		"subject": {Type: "string", Required: true},
		"count":   {Type: "int", Required: true},
		"deep":    {Type: "bool", Default: false},
		"mode":    {Type: "string", Enum: []string{"fast", "safe"}, Default: "safe"},
		"extra":   {Type: "object"},
	}

	t.Run("coercion and defaults", func(t *testing.T) {
		got, err := CheckParams(decl, map[string]string{
			"subject": "alpha", "count": "3", "extra": `{"k": 1}`,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got["count"].Value != 3 {
			t.Fatalf("int not coerced: %+v", got["count"])
		}
		if got["deep"].Value != false || got["mode"].Value != "safe" {
			t.Fatalf("defaults not applied: %+v", got)
		}
		obj, ok := got["extra"].Value.(map[string]any)
		if !ok || obj["k"] != float64(1) {
			t.Fatalf("object param not parsed as JSON: %+v", got["extra"])
		}
	})

	t.Run("missing required is a hard error", func(t *testing.T) {
		_, err := CheckParams(decl, map[string]string{"subject": "alpha"})
		wantErrContaining(t, err, "params.count: required param missing")
	})

	t.Run("empty string is presence, not truthiness", func(t *testing.T) {
		got, err := CheckParams(decl, map[string]string{"subject": "", "count": "0"})
		if err != nil {
			t.Fatalf("empty string for a required param must be accepted: %v", err)
		}
		if got["subject"].Value != "" {
			t.Fatalf("want empty string value, got %+v", got["subject"])
		}
	})

	t.Run("unknown param lists declared params", func(t *testing.T) {
		_, err := CheckParams(decl, map[string]string{"subject": "a", "count": "1", "surprise": "x"})
		wantErrContaining(t, err, "params.surprise: unknown param (declared params: count, deep, extra, mode, subject)")
	})

	t.Run("all violations joined", func(t *testing.T) {
		_, err := CheckParams(decl, map[string]string{"count": "many", "mode": "wild"})
		wantErrContaining(t, err, `params.count: value "many" is not an int`)
		wantErrContaining(t, err, `params.mode: value "wild" not in enum [fast, safe]`)
		wantErrContaining(t, err, "params.subject: required param missing")
	})
}

// Verifies 255893ae16eb: repo is an ordinary optional param, never special —
// the reference declares it as a plain workflow param and templates consume it
// as an ordinary input slot, overridable per step.
func TestRepoIsAnOrdinaryParam(t *testing.T) {
	cfg := loadRef(t)
	decl, ok := cfg.Workflows["task"].Params["repo"]
	if !ok {
		t.Fatal("repo must be declared as an ordinary workflow param")
	}
	if decl.Type != "string" {
		t.Fatalf("repo is an ordinary string param, got %+v", decl)
	}
	// A per-step literal override desugars like any other binding.
	cfg.Workflows["task"].Steps[0].With["repo"] = "other-checkout"
	ir, err := Desugar(cfg, "task")
	if err != nil {
		t.Fatalf("desugar with per-step repo override: %v", err)
	}
	for _, n := range ir.Nodes {
		if n.ID == "task/implement" {
			b := n.Bindings["repo"]
			if b.Kind != BindLiteral || b.Value != "other-checkout" {
				t.Fatalf("per-step repo override must desugar to a literal binding, got %+v", b)
			}
		}
	}
	if err := CheckWiring(ir, cfg); err != nil {
		t.Fatalf("override must pass wiring: %v", err)
	}
}

// Verifies 23a6be447cc8: name-discipline diagnostics are emitted in a fixed
// section order, deterministic across runs (fix pass finding 3).
func TestSchemaNameDisciplineDeterministic(t *testing.T) {
	build := func() *Config {
		cfg := minimalConfig()
		cfg.Identities[""] = IdentityDef{Key: "./keys/x"}
		cfg.Templates[""] = TemplateDef{Build: &BuildDef{Packages: []string{"git"}}, Skill: "act"}
		cfg.Workflows[""] = WorkflowDef{Steps: []StepDef{{ID: "s", Use: "box"}}}
		return cfg
	}
	first := Validate(build(), nil).Error()
	for i := 0; i < 50; i++ {
		if got := Validate(build(), nil).Error(); got != first {
			t.Fatalf("error order changed across runs:\n%s\n---\n%s", first, got)
		}
	}
	if !strings.Contains(first, "identities: empty name") || !strings.Contains(first, "templates: empty name") {
		t.Fatalf("missing empty-name violations:\n%s", first)
	}
	idx := func(s string) int { return strings.Index(first, s) }
	if !(idx("identities: empty name") < idx("templates: empty name") && idx("templates: empty name") < idx("workflows: empty name")) {
		t.Fatalf("sections must be reported in fixed order:\n%s", first)
	}
}

// Verifies 23a6be447cc8 and 8a79b4f5c699: a ${...} nested inside a compound
// with: literal is rejected with a field-path error instead of silently
// passing through as literal text (fix pass finding 4).
func TestSchemaNestedInterpolationRejected(t *testing.T) {
	t.Run("nested in a map literal", func(t *testing.T) {
		cfg := minimalConfig()
		tp := cfg.Templates["box"]
		tp.Inputs = map[string]ParamDef{"input": {Type: "object", Required: true}}
		cfg.Templates["box"] = tp
		cfg.Workflows["flow"].Steps[0].With["input"] = map[string]any{"inner": "${params.subject}"}
		err := Validate(cfg, nil)
		wantErrContaining(t, err, "workflows.flow.steps[0].with.input")
		wantErrContaining(t, err, "only legal as a whole scalar binding value")
	})

	t.Run("nested in a list literal", func(t *testing.T) {
		cfg := minimalConfig()
		tp := cfg.Templates["box"]
		tp.Inputs = map[string]ParamDef{"input": {Type: "object", Required: true}}
		cfg.Templates["box"] = tp
		cfg.Workflows["flow"].Steps[0].With["input"] = []any{"plain", []any{"${steps.first.result}"}}
		err := Validate(cfg, nil)
		wantErrContaining(t, err, "workflows.flow.steps[0].with.input")
		wantErrContaining(t, err, "only legal as a whole scalar binding value")
	})

	t.Run("compound literal without references passes", func(t *testing.T) {
		cfg := minimalConfig()
		tp := cfg.Templates["box"]
		tp.Inputs = map[string]ParamDef{"input": {Type: "object", Required: true}}
		cfg.Templates["box"] = tp
		cfg.Workflows["flow"].Steps[0].With["input"] = map[string]any{"inner": []any{"plain", 1}}
		if err := Validate(cfg, nil); err != nil {
			t.Fatalf("plain compound literal must pass: %v", err)
		}
	})
}

// Verifies 255893ae16eb: param and field defaults are type-checked against the
// declaration, including enum membership (fix pass finding 5).
func TestSchemaParamDefaultTyping(t *testing.T) {
	tests := []struct {
		name    string
		def     ParamDef
		wantErr string
	}{
		{"int default on a string param", ParamDef{Type: "string", Default: 5}, "workflows.flow.params.p.default: default is int, declared type is string"},
		{"string default on a bool param", ParamDef{Type: "bool", Default: "yes"}, "workflows.flow.params.p.default: default is string, declared type is bool"},
		{"default outside the enum", ParamDef{Type: "string", Enum: []string{"fast", "safe"}, Default: "wild"}, "workflows.flow.params.p.default: default wild not in enum [fast, safe]"},
		{"well-typed default", ParamDef{Type: "int", Default: 5}, ""},
		{"enum member default", ParamDef{Type: "string", Enum: []string{"fast", "safe"}, Default: "safe"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalConfig()
			wf := cfg.Workflows["flow"]
			wf.Params = map[string]ParamDef{
				"subject": {Type: "string", Required: true},
				"p":       tt.def,
			}
			cfg.Workflows["flow"] = wf
			err := Validate(cfg, nil)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			wantErrContaining(t, err, tt.wantErr)
		})
	}
}

// Verifies 23a6be447cc8 (F4 validation altitude): the identifier grammars —
// step ids exclude the node-id namespacing characters; slot/param names are
// env-embeddable and token-disjoint; credential service names cannot shadow
// engine- or runner-owned variables; and the run-contract mirrors (agent CLI
// named, engine-owned template env, reserved volume paths) fail validate,
// not each step at run time.
func TestValidateIdentifierGrammars(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			"step id with a namespacing character",
			func(c *Config) {
				wf := c.Workflows["flow"]
				wf.Steps[0].ID = "first/nested"
				c.Workflows["flow"] = wf
			},
			`invalid step id "first/nested"`,
		},
		{
			"step id with an iteration marker",
			func(c *Config) {
				wf := c.Workflows["flow"]
				wf.Steps[0].ID = "first@2"
				c.Workflows["flow"] = wf
			},
			`invalid step id "first@2"`,
		},
		{
			"param name outside the env-safe grammar",
			func(c *Config) {
				c.Workflows["flow"].Params["bad name"] = ParamDef{Type: "string"}
			},
			`params.bad name: invalid name`,
		},
		{
			"slot names colliding on the env token",
			func(c *Config) {
				c.Templates["box"].Inputs["in-put"] = ParamDef{Type: "string"}
				c.Templates["box"].Inputs["in_put"] = ParamDef{Type: "string"}
			},
			`both map to the env token IN_PUT`,
		},
		{
			"step id with a dot is unreferenceable",
			func(c *Config) {
				wf := c.Workflows["flow"]
				wf.Steps[0].ID = "first.stage"
				c.Workflows["flow"] = wf
			},
			`invalid step id "first.stage"`,
		},
		{
			"credential services colliding on the env token",
			func(c *Config) {
				c.Credentials.Services = map[string]ServiceDef{
					"agent-api": {Mode: "file"}, "agent_api": {Mode: "file"},
				}
			},
			`collides with "agent-api": both export the env variable AGENT_API`,
		},
		{
			"template volume over the HOME tmpfs",
			func(c *Config) {
				tpl := c.Templates["box"]
				tpl.Run.Volumes = map[string]string{"h": "/home/box"}
				c.Templates["box"] = tpl
			},
			`overlaps the reserved path "/home/box"`,
		},
		{
			"template volume keyed by a host path",
			func(c *Config) {
				tpl := c.Templates["box"]
				tpl.Run.Volumes = map[string]string{"/host/cache": "/cache"}
				c.Templates["box"] = tpl
			},
			`run.volumes./host/cache: host paths are not mounted; declare a named volume`,
		},
		{
			"template volume keyed by a relative path",
			func(c *Config) {
				tpl := c.Templates["box"]
				tpl.Run.Volumes = map[string]string{"./cache": "/cache"}
				c.Templates["box"] = tpl
			},
			`host paths are not mounted`,
		},
		{
			"template volume with a bad name",
			func(c *Config) {
				tpl := c.Templates["box"]
				tpl.Run.Volumes = map[string]string{"bad name": "/cache"}
				c.Templates["box"] = tpl
			},
			`not a docker volume name`,
		},
		{
			"template volume with a relative container path",
			func(c *Config) {
				tpl := c.Templates["box"]
				tpl.Run.Volumes = map[string]string{"cache": "cache"}
				c.Templates["box"] = tpl
			},
			`container path "cache" must be absolute`,
		},
		{
			"template volume with a colon in the container path",
			func(c *Config) {
				tpl := c.Templates["box"]
				tpl.Run.Volumes = map[string]string{"cache": "/a:b"}
				c.Templates["box"] = tpl
			},
			`may not contain a colon`,
		},
		{
			"credential service shadowing PATH",
			func(c *Config) {
				c.Credentials.Services = map[string]ServiceDef{"path": {Mode: "file"}}
			},
			`exports the engine- or runner-owned variable PATH`,
		},
		{
			"missing agent CLI",
			func(c *Config) {
				tpl := c.Templates["box"]
				tpl.Run.Env = nil
				c.Templates["box"] = tpl
			},
			`run.env.FABER_AGENT_CLI: required`,
		},
		{
			"engine-owned template env",
			func(c *Config) {
				c.Templates["box"].Run.Env["FABER_SKILL"] = "evil"
			},
			`run.env.FABER_SKILL: engine- or security-owned name`,
		},
		{
			"template volume over a reserved path",
			func(c *Config) {
				tpl := c.Templates["box"]
				tpl.Run.Volumes = map[string]string{"x": "/faber/hooks"}
				c.Templates["box"] = tpl
			},
			`overlaps the reserved path "/faber"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalConfig()
			tt.mutate(cfg)
			err := Validate(cfg, nil)
			wantErrContaining(t, err, tt.wantErr)
		})
	}
}

// Verifies 9bdc00533e50: lanes, a template's lane, and a generate's
// max_parallel are schema fields that validate and reach the IR; an unknown
// lane, a lane below 1, and a negative max_parallel are loader violations;
// lanes are substrate (root-only across includes); absent, the IR is
// byte-identical to before the fields existed.
func TestSchemaConcurrencyDeclarations(t *testing.T) {
	cfg := minimalConfig()
	cfg.Lanes = map[string]int{"landing": 1}
	box := cfg.Templates["box"]
	box.Lane = "landing"
	cfg.Templates["box"] = box
	cfg.Workflows["per"] = WorkflowDef{
		Params: map[string]ParamDef{"input": {Type: "string", Required: true}},
		Steps:  []StepDef{{ID: "one", Use: "box", With: map[string]any{"input": "${params.input}"}}},
	}
	wf := cfg.Workflows["flow"]
	wf.Sources = map[string]SourceDef{"feed": {Command: "./list"}}
	wf.Steps = append(wf.Steps, StepDef{ID: "fan", Generate: &GenerateDef{Source: "feed", Workflow: "per", With: map[string]any{"input": "${item.id}"}, MaxParallel: 3}})
	cfg.Workflows["flow"] = wf
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("valid concurrency declarations rejected: %v", err)
	}
	ir, err := Desugar(cfg, "flow")
	if err != nil {
		t.Fatal(err)
	}
	if ir.Lanes["landing"] != 1 {
		t.Errorf("IR lanes = %v, want landing=1", ir.Lanes)
	}
	var gotLane, gotMax bool
	for _, n := range ir.Nodes {
		if n.Template != nil && n.Template.Lane == "landing" {
			gotLane = true
		}
		if n.Gen != nil && n.Gen.MaxParallel == 3 {
			gotMax = true
		}
	}
	if !gotLane || !gotMax {
		t.Errorf("IR lacks the lane (%v) or the generate bound (%v)", gotLane, gotMax)
	}
	if !cfg.hasSubstrate() {
		t.Error("lanes must count as substrate (root-only)")
	}

	for _, tt := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"unknown lane", func(c *Config) { b := c.Templates["box"]; b.Lane = "ghost"; c.Templates["box"] = b }, `templates.box.lane: unknown lane "ghost"`},
		{"lane below 1", func(c *Config) { c.Lanes["landing"] = 0 }, "lanes.landing: must be >= 1"},
		{"negative max_parallel", func(c *Config) { c.Workflows["flow"].Steps[1].Generate.MaxParallel = -1 }, "workflows.flow.steps[1].generate.max_parallel: must be >= 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := minimalConfig()
			c.Lanes = map[string]int{"landing": 1}
			b := c.Templates["box"]
			b.Lane = "landing"
			c.Templates["box"] = b
			c.Workflows["per"] = cfg.Workflows["per"]
			w := c.Workflows["flow"]
			w.Sources = map[string]SourceDef{"feed": {Command: "./list"}}
			w.Steps = append(w.Steps, StepDef{ID: "fan", Generate: &GenerateDef{Source: "feed", Workflow: "per", With: map[string]any{"input": "${item.id}"}}})
			c.Workflows["flow"] = w
			tt.mutate(c)
			wantErrContaining(t, Validate(c, nil), tt.wantErr)
		})
	}

	plain := minimalConfig()
	pir, err := Desugar(plain, "flow")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeIR(pir)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{`"lanes"`, `"lane"`, `"max_parallel"`} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("IR without declarations carries %s", banned)
		}
	}
}

// Verifies 6fc8922b8a41: a loop's expect names a declared source and reaches
// the IR's loop table; an unknown source is a violation; HashIRCapFree is
// equal across caps and different across any other change; an IR without
// expectations carries no loop table.
func TestSchemaLoopExpectAndCapFreeHash(t *testing.T) {
	withLoop := func(max int, expect string) *Config {
		c := minimalConfig()
		wf := c.Workflows["flow"]
		wf.Sources = map[string]SourceDef{"todo": {Command: "./count", Args: []string{"--subject", "${params.subject}"}}}
		wf.Steps = append(wf.Steps, StepDef{ID: "cycle", Loop: &LoopDef{Max: max, Until: "true", Expect: expect, Steps: []StepDef{
			{ID: "inner", Use: "box", With: map[string]any{"input": "x"}},
		}}})
		c.Workflows["flow"] = wf
		return c
	}
	cfg := withLoop(3, "todo")
	if err := Validate(cfg, nil); err != nil {
		t.Fatalf("valid expect rejected: %v", err)
	}
	ir, err := Desugar(cfg, "flow")
	if err != nil {
		t.Fatal(err)
	}
	if len(ir.Loops) != 1 || ir.Loops[0].ID != "flow/cycle" || ir.Loops[0].Max != 3 || ir.Loops[0].Command != "./count" || len(ir.Loops[0].Args) != 2 {
		t.Errorf("loop table = %+v", ir.Loops)
	}
	wantErrContaining(t, Validate(withLoop(3, "ghost"), nil), `workflows.flow.steps[1].loop.expect: unknown source "ghost"`)

	h3, err := HashIRCapFree(withLoop(3, "todo"), "flow")
	if err != nil {
		t.Fatal(err)
	}
	h5, err := HashIRCapFree(withLoop(5, "todo"), "flow")
	if err != nil {
		t.Fatal(err)
	}
	if h3 != h5 {
		t.Error("cap-free hash must not change with the cap")
	}
	full3, _ := HashIR(ir)
	ir5, _ := Desugar(withLoop(5, "todo"), "flow")
	full5, _ := HashIR(ir5)
	if full3 == full5 {
		t.Error("the full hash must change with the cap (the cap-free key exists for a reason)")
	}
	changed := withLoop(3, "todo")
	changed.Workflows["flow"].Steps[1].Loop.Steps[0].ID = "other"
	hc, err := HashIRCapFree(changed, "flow")
	if err != nil {
		t.Fatal(err)
	}
	if hc == h3 {
		t.Error("a changed body step must change the cap-free hash")
	}
	plain, _ := Desugar(withLoop(3, ""), "flow")
	raw, _ := EncodeIR(plain)
	if strings.Contains(string(raw), `"loops"`) {
		t.Error("an IR without expectations must carry no loop table")
	}
}
