package config

import (
	"fmt"
	"strings"
	"testing"
)

// invokeBase parametrizes the shared fixture over the invoke_profiles library
// and the template's invoke: block.
const invokeBase = `
version: 1
identities:
  worker: {key: ./keys/worker}
%s
templates:
  box:
    build: {packages: [git]}
    run: {identity: worker, env: {FABER_AGENT_CLI: agent-cli}}
    skill: act
    model: agent-model
    effort: high
    %s
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps:
      - id: first
        use: box
        with: {input: "${params.subject}"}
`

// Verifies spec/proposals/2026-08-14-invoke-profile.md scenario 11: an
// invoke: block resolves to the fully concrete layered profile — inline
// overrides over the named profile over the built-in default, with the
// explicit-empty sentinels honored — and a template without one resolves to
// nil with IR bytes unchanged from before the field existed.
func TestInvokeProfileResolution(t *testing.T) {
	profiles := `
invoke_profiles:
  goose:
    subcommand: [run]
    prompt_flag: ""
    skill_mode: flag
    skill_flag: --recipe
    prompt_template: "{body}{extra}"
    fixed_flags: []
    effort_flag: ""
`
	cfg, err := loadStr(t, fmt.Sprintf(invokeBase, profiles,
		"invoke: {profile: goose, budget_flag: --cost-cap}"))
	if err != nil {
		t.Fatalf("must validate: %v", err)
	}
	got := resolvedBox(t, cfg).Invoke
	if got == nil {
		t.Fatal("invoke: must resolve into the template")
	}
	want := ResolvedInvoke{
		Subcommand:     []string{"run"},
		PromptFlag:     "", // explicit "" beats the default -p
		SkillMode:      SkillModeFlag,
		SkillFlag:      "--recipe",
		PromptTemplate: "{body}{extra}",
		FixedFlags:     nil,          // explicit [] beats the default bypass tail
		ModelFlag:      "--model",    // untouched layer: the built-in default
		EffortFlag:     "",           // explicit "" drops the pair
		BudgetFlag:     "--cost-cap", // inline override beats profile and default
	}
	if fmt.Sprint(*got) != fmt.Sprint(want) {
		t.Fatalf("resolved invoke = %+v, want %+v", *got, want)
	}
}

// Verifies scenario 11: a template without invoke: emits IR bytes identical
// to a config that never heard of the field, while adding the block changes
// the bytes (the hash resume guards on).
func TestInvokeProfileByteStableWhenAbsent(t *testing.T) {
	without, err := loadStr(t, fmt.Sprintf(invokeBase, "", "hooks: {}"))
	if err != nil {
		t.Fatalf("must validate: %v", err)
	}
	with, err := loadStr(t, fmt.Sprintf(invokeBase, "",
		`invoke: {skill_mode: flag, skill_flag: --recipe, prompt_template: "{body}"}`))
	if err != nil {
		t.Fatalf("must validate: %v", err)
	}
	encode := func(cfg *Config) string {
		t.Helper()
		ir, err := Desugar(cfg, "flow")
		if err != nil {
			t.Fatal(err)
		}
		b, err := EncodeIR(ir)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if s := encode(without); strings.Contains(s, "invoke") {
		t.Fatalf("an absent invoke: must serialize to nothing:\n%s", s)
	}
	if s := encode(with); !strings.Contains(s, `"skill_flag": "--recipe"`) {
		t.Fatalf("a declared invoke: must be carried in the IR:\n%s", s)
	}
}

// Verifies scenario 11: an inline-only invoke: (no profile name) overrides
// the built-in default directly.
func TestInvokeProfileInlineOnly(t *testing.T) {
	cfg, err := loadStr(t, fmt.Sprintf(invokeBase, "", "invoke: {budget_flag: --cost-cap}"))
	if err != nil {
		t.Fatalf("must validate: %v", err)
	}
	got := resolvedBox(t, cfg).Invoke
	if got.BudgetFlag != "--cost-cap" || got.PromptFlag != "-p" || got.SkillMode != SkillModePrefix {
		t.Fatalf("resolved invoke = %+v, want the default with only the budget flag overridden", *got)
	}
}

// Verifies scenario 11: the validate-time rule catalog, field-pathed, both
// per referencing template and standalone per library entry.
func TestInvokeProfileValidation(t *testing.T) {
	tests := []struct {
		name, profiles, invoke, wantPath, wantMsg string
	}{
		{
			name:     "unknown profile name",
			invoke:   "invoke: {profile: goos}",
			profiles: "invoke_profiles:\n  goose: {skill_mode: prefix}",
			wantPath: "templates.box.invoke.profile",
			wantMsg:  `unknown invoke profile "goos"`,
		},
		{
			name:     "unknown skill mode",
			invoke:   "invoke: {skill_mode: banner}",
			wantPath: "templates.box.invoke.skill_mode",
			wantMsg:  `unknown mode "banner"`,
		},
		{
			name:     "prompt template must carry the body",
			invoke:   `invoke: {prompt_template: "/{skill}"}`,
			wantPath: "templates.box.invoke.prompt_template",
			wantMsg:  "must contain {body}",
		},
		{
			name:     "flag mode needs a skill flag",
			invoke:   `invoke: {skill_mode: flag, prompt_template: "{body}"}`,
			wantPath: "templates.box.invoke.skill_flag",
			wantMsg:  "required with skill_mode",
		},
		{
			name:     "flag mode with the default template double-injects",
			invoke:   "invoke: {skill_mode: flag, skill_flag: --recipe}",
			wantPath: "templates.box.invoke.prompt_template",
			wantMsg:  "forbids {skill}",
		},
		{
			name:     "prefix mode with a skill flag",
			invoke:   "invoke: {skill_flag: --recipe}",
			wantPath: "templates.box.invoke.skill_flag",
			wantMsg:  "only meaningful with skill_mode",
		},
		{
			name:     "prefix mode must reach the skill",
			invoke:   `invoke: {prompt_template: "{body}"}`,
			wantPath: "templates.box.invoke.prompt_template",
			wantMsg:  "requires {skill}",
		},
		{
			name:     "library entry checked standalone though unreferenced",
			profiles: "invoke_profiles:\n  broken: {skill_mode: flag}",
			invoke:   "hooks: {}",
			wantPath: "invoke_profiles.broken.skill_flag",
			wantMsg:  "required with skill_mode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadStr(t, fmt.Sprintf(invokeBase, tt.profiles, tt.invoke))
			if err == nil {
				t.Fatal("must not validate")
			}
			want := tt.wantPath + ": "
			if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %q, want path %q with %q", err, want, tt.wantMsg)
			}
		})
	}
}

// Verifies scenario 11: an inline override can take a flag-mode profile back
// to prefix mode — skill_flag's pointer carrier makes the explicit "" clear
// the inherited flag, which the prefix-mode rules then require.
func TestInvokeProfileInlineClearsSkillFlag(t *testing.T) {
	profiles := `
invoke_profiles:
  goose: {skill_mode: flag, skill_flag: --recipe, prompt_template: "{body}"}
`
	invoke := `invoke: {profile: goose, skill_mode: prefix, skill_flag: "", prompt_template: "/{skill}\n\n{body}{extra}"}`
	cfg, err := loadStr(t, fmt.Sprintf(invokeBase, profiles, invoke))
	if err != nil {
		t.Fatalf("must validate: %v", err)
	}
	got := resolvedBox(t, cfg).Invoke
	if got.SkillMode != SkillModePrefix || got.SkillFlag != "" {
		t.Fatalf("resolved invoke = %+v, want prefix mode with the inherited skill flag cleared", *got)
	}
}

// Verifies scenario 11: an inline override may legitimately complete a
// partial named profile — the rules bind on the EFFECTIVE value.
func TestInvokeProfileInlineCompletesNamed(t *testing.T) {
	profiles := `
invoke_profiles:
  goose: {skill_mode: flag, skill_flag: --recipe, prompt_template: "{body}"}
`
	cfg, err := loadStr(t, fmt.Sprintf(invokeBase, profiles,
		`invoke: {profile: goose, prompt_template: "context: {body}"}`))
	if err != nil {
		t.Fatalf("must validate: %v", err)
	}
	if got := resolvedBox(t, cfg).Invoke.PromptTemplate; got != "context: {body}" {
		t.Fatalf("prompt template = %q, want the inline override", got)
	}
}
