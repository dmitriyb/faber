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
	cfg, err := Load("testdata/reference.yaml")
	if err != nil {
		t.Fatalf("load reference: %v", err)
	}
	if err := Validate(cfg); err != nil {
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
			err := Validate(cfg)
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
				Build:  BuildDef{Packages: []string{"git"}},
				Run:    RunDef{Identity: "worker"},
				Skill:  "act",
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalConfig()
			tt.mutate(cfg)
			err := Validate(cfg)
			wantErrContaining(t, err, tt.wantErr)
			if n := errCount(err); n != 1 {
				t.Fatalf("want exactly one violation, got %d: %v", n, err)
			}
		})
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
