package config

import (
	"strings"
	"testing"
)

// loadStr writes a single-file config into a shared dir and assembles it (no
// validation), returning the Config and any validate error for negative tests.
func loadStr(t *testing.T, body string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	path := writeFile(t, dir, "orchestrator.yaml", body)
	cfg, viols, err := Load(path)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	return cfg, Validate(cfg, viols)
}

const namedForm = `
version: 1
identities:
  worker: {key: ./keys/worker}
images:
  base: {packages: [git], overlay: ./o.nix}
hooks:
  ctx: {path: ./hooks/ctx}
templates:
  box:
    image: base
    identity: worker
    skill: act
    hooks: {context: ctx}
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

const inlineForm = `
version: 1
identities:
  worker: {key: ./keys/worker}
templates:
  box:
    build: {packages: [git], overlay: ./o.nix}
    run: {identity: worker}
    skill: act
    hooks: {context: ./hooks/ctx}
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

// Scenario 13: named image/hooks/identity and the inline build/hooks-paths/
// run.identity forms, with identical underlying values, desugar to byte-
// identical IR for those aspects.
func TestDualModeEquivalence(t *testing.T) {
	named, err := loadStr(t, namedForm)
	if err != nil {
		t.Fatalf("named form must validate: %v", err)
	}
	inline, err := loadStr(t, inlineForm)
	if err != nil {
		t.Fatalf("inline form must validate: %v", err)
	}
	nir, err := Desugar(named, "flow")
	if err != nil {
		t.Fatalf("desugar named: %v", err)
	}
	iir, err := Desugar(inline, "flow")
	if err != nil {
		t.Fatalf("desugar inline: %v", err)
	}
	a, _ := EncodeIR(nir)
	b, _ := EncodeIR(iir)
	if string(a) != string(b) {
		t.Fatalf("named and inline forms must resolve identically:\n%s\n---\n%s", a, b)
	}
}

// resolvedBox desugars flow and returns the resolved box template.
func resolvedBox(t *testing.T, cfg *Config) *ResolvedTemplate {
	t.Helper()
	ir, err := Desugar(cfg, "flow")
	if err != nil {
		t.Fatalf("desugar: %v", err)
	}
	for i := range ir.Nodes {
		if ir.Nodes[i].Template != nil {
			return ir.Nodes[i].Template
		}
	}
	t.Fatal("no agent node")
	return nil
}

// Scenario 14: named skills desugar to an ordered, name-deduped Sources set
// (empty Root); the inline {dir, link} form yields Root with empty Sources (a
// direct mount, no <name> wrapper, no double-nesting).
func TestSkillsNamedVersusInlineShapes(t *testing.T) {
	named, err := loadStr(t, `
version: 1
images: {base: {packages: [git]}}
skills:
  implement: {dir: ./skills/implement}
  go-expert: {dir: ./skills/go}
templates:
  box:
    image: base
    skill: implement
    skills: [implement, go-expert, implement]
    skills_link: .claude/skills
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`)
	if err != nil {
		t.Fatalf("named skills must validate: %v", err)
	}
	rs := resolvedBox(t, named).Skills
	if rs == nil || rs.Root != "" || len(rs.Sources) != 2 {
		t.Fatalf("named skills must yield 2 deduped Sources and empty Root, got %+v", rs)
	}
	if rs.Sources[0].Name != "implement" || rs.Sources[1].Name != "go-expert" {
		t.Fatalf("Sources must preserve declared order (deduped), got %+v", rs.Sources)
	}
	if rs.Primary != "implement" || rs.Link != ".claude/skills" {
		t.Fatalf("named skills primary/link wrong: %+v", rs)
	}

	inline, err := loadStr(t, `
version: 1
images: {base: {packages: [git]}}
templates:
  box:
    image: base
    skill: summarize
    skills: {dir: ./skills, link: .claude/skills}
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`)
	if err != nil {
		t.Fatalf("inline skills must validate: %v", err)
	}
	rs = resolvedBox(t, inline).Skills
	if rs == nil || rs.Root != "./skills" || len(rs.Sources) != 0 {
		t.Fatalf("inline skills must yield Root and empty Sources, got %+v", rs)
	}
}

// A skills: key present with a null/empty value means the aspect is absent — no
// skills leg, no parse error (regression: the empty scalar used to hit the
// default branch and hard-error). A non-null scalar still errors cleanly.
func TestSkillsNullMeansAbsent(t *testing.T) {
	cfg, err := loadStr(t, `
version: 1
images: {base: {packages: [git]}}
templates:
  box:
    image: base
    skill: act
    skills:
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`)
	if err != nil {
		t.Fatalf("null skills: must validate with no skills leg: %v", err)
	}
	if rs := resolvedBox(t, cfg).Skills; rs != nil {
		t.Fatalf("null skills: must yield no skills leg, got %+v", rs)
	}

	// A non-null scalar is still a type error, surfaced at parse (Load) time.
	dir := t.TempDir()
	path := writeFile(t, dir, "orchestrator.yaml", `
version: 1
templates:
  box:
    skill: act
    skills: nonsense
`)
	if _, _, err := Load(path); err == nil {
		t.Fatal("a non-null scalar skills: value must error")
	}
}

// Scenario 10: a free-form skill: with no skills: library (or an inline mapping)
// is a plain prompt token — no membership or existence check. This is the case
// every current config hits and must keep validating.
func TestFreeFormSkillValid(t *testing.T) {
	// No skills library, skill: is free-form.
	if _, err := loadStr(t, inlineForm); err != nil {
		t.Fatalf("skill: without a skills library must validate: %v", err)
	}
	// Inline skills mapping — skill: is still free-form (not in the library).
	_, err := loadStr(t, `
version: 1
images: {base: {packages: [git]}}
templates:
  box:
    image: base
    skill: anything-goes
    skills: {dir: ./skills, link: .claude/skills}
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`)
	if err != nil {
		t.Fatalf("free-form skill with inline skills must validate: %v", err)
	}
}

// Scenario 10 (named mode): the primary skill must be one of the delivered
// names in named mode; a non-member fails.
func TestPrimarySkillMustBeDeliveredInNamedMode(t *testing.T) {
	_, err := loadStr(t, `
version: 1
images: {base: {packages: [git]}}
skills: {implement: {dir: ./s}}
templates:
  box:
    image: base
    skill: review
    skills: [implement]
    skills_link: .claude/skills
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`)
	if err == nil || !strings.Contains(err.Error(), "templates.box.skill") {
		t.Fatalf("primary skill outside the delivered set must fail, got %v", err)
	}
}

// Scenario 11: dual-mode conflicts are field-pathed exclusivity errors.
func TestDualModeConflicts(t *testing.T) {
	base := func(body string) string {
		return "version: 1\nimages: {base: {packages: [git]}}\nidentities: {worker: {key: ./k}}\n" + body +
			"\nworkflows:\n  flow:\n    params: {subject: {type: string, required: true}}\n    steps: [{id: first, use: box, with: {input: \"${params.subject}\"}}]\n"
	}
	tests := []struct {
		name, body, want string
	}{
		{
			"image and build both set",
			base("templates:\n  box:\n    image: base\n    build: {packages: [git]}\n    skill: a\n    inputs: {input: {type: string, required: true}}\n    output: {result: {type: string, required: true}}\n"),
			"templates.box: image and build are mutually exclusive",
		},
		{
			"identity and run.identity both set",
			base("templates:\n  box:\n    image: base\n    identity: worker\n    run: {identity: worker}\n    skill: a\n    inputs: {input: {type: string, required: true}}\n    output: {result: {type: string, required: true}}\n"),
			"templates.box: identity and run.identity are mutually exclusive",
		},
		{
			"skills_link with inline mapping",
			base("templates:\n  box:\n    image: base\n    skill: a\n    skills: {dir: ./s, link: .claude/skills}\n    skills_link: .claude/skills\n    inputs: {input: {type: string, required: true}}\n    output: {result: {type: string, required: true}}\n"),
			"templates.box.skills_link: must not be set with an inline skills mapping",
		},
		{
			"skills_link with no skills leg",
			base("templates:\n  box:\n    image: base\n    skill: a\n    skills_link: .claude/skills\n    inputs: {input: {type: string, required: true}}\n    output: {result: {type: string, required: true}}\n"),
			"templates.box.skills_link: set without a skills leg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadStr(t, tt.body); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

// Scenario 9 (wiring): every dangling library reference fails validate with its
// field-pathed error, and all are collected in one report.
func TestDanglingLibraryRefsCollected(t *testing.T) {
	_, err := loadStr(t, `
version: 1
images: {base: {packages: [git]}}
skills: {real: {dir: ./s}}
hooks: {real: {path: ./hooks/real}}
identities: {worker: {key: ./k}}
templates:
  box:
    image: ghostimage
    identity: ghostid
    skill: real
    skills: [real, ghostskill]
    skills_link: .claude/skills
    hooks: {context: ghosthook}
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`)
	if err == nil {
		t.Fatal("dangling refs must fail validate")
	}
	joined := err.Error()
	for _, want := range []string{
		`templates.box.image: unknown image "ghostimage"`,
		`templates.box.identity: unknown identity "ghostid"`,
		`templates.box.hooks.context: unknown hook "ghosthook"`,
		`templates.box.skills[1]: unknown skill "ghostskill"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("collected report missing %q:\n%s", want, joined)
		}
	}
}

// Name discipline: a library key and a referenced skill name are joined into a
// host path when staging (<stage>/<name>), so a traversal/"/"-carrying name must
// FAIL faber validate — never reach copyTree. Both the declaring library key and
// the template.skills[*] reference are field-pathed, collected in one report.
func TestUnsafeSkillNameRejected(t *testing.T) {
	_, err := loadStr(t, `
version: 1
images: {base: {packages: [git]}}
skills:
  "../../etc/x": {dir: ./s}
templates:
  box:
    image: base
    skill: "../../etc/x"
    skills: ["../../etc/x"]
    skills_link: .claude/skills
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`)
	if err == nil {
		t.Fatal("a traversal skill name must fail validate")
	}
	joined := err.Error()
	for _, want := range []string{
		`skills."../../etc/x": name must be a safe identifier`,
		`templates.box.skills[0]: skill name must be a safe identifier`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("collected report missing %q:\n%s", want, joined)
		}
	}
}
