package config

import (
	"strings"
	"testing"
)

// pinConfig wraps a template body carrying the given image/build section into a
// full single-file config with a "box" template and a "flow" workflow so
// resolvedBox can desugar it.
func pinNamedConfig(imagesBody, templateBody string) string {
	return `
version: 1
images:
` + imagesBody + `
templates:
  box:
` + templateBody + `
    skill: act
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`
}

// A named image's pin survives ResolveBuild's ImageDef→BuildDef projection and
// lands on the flat ResolvedTemplate.Pin (the explicit img.Pin copy). The inline
// build.pin form carries identically, and both forms desugar to a byte-identical
// ResolvedTemplate.Pin.
func TestPinCarriesThroughNamedAndInline(t *testing.T) {
	named, err := loadStr(t, pinNamedConfig(
		`  base: {packages: [git], pin: {rev: "25.11", sha256: "sha256:abc"}}`,
		`    image: base`))
	if err != nil {
		t.Fatalf("named pin must validate: %v", err)
	}
	np := resolvedBox(t, named).Pin
	if np == nil {
		t.Fatal("named image's pin was dropped (ResolveBuild did not copy img.Pin)")
	}
	if np.Rev != "25.11" || np.SHA256 != "sha256:abc" {
		t.Fatalf("named pin wrong: %+v", np)
	}

	inline, err := loadStr(t, `
version: 1
templates:
  box:
    build: {packages: [git], pin: {rev: "25.11", sha256: "sha256:abc"}}
    skill: act
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`)
	if err != nil {
		t.Fatalf("inline pin must validate: %v", err)
	}
	ip := resolvedBox(t, inline).Pin
	if ip == nil {
		t.Fatal("inline build.pin was dropped")
	}
	if *ip != *np {
		t.Fatalf("named and inline forms must yield identical ResolvedTemplate.Pin: %+v vs %+v", ip, np)
	}
}

// A fully-empty pin: {} normalizes to nil (absent) and passes validation; a
// template that declares no pin resolves to ResolvedTemplate.Pin == nil, and its
// IR omits the pin field entirely (byte-stable). A pinned template's IR carries
// the pin field.
func TestEmptyAndAbsentPinNormalizeToNilAndOmit(t *testing.T) {
	empty, err := loadStr(t, pinNamedConfig(
		`  base: {packages: [git], pin: {}}`,
		`    image: base`))
	if err != nil {
		t.Fatalf("empty pin: {} must validate: %v", err)
	}
	if img := empty.Images["base"]; img.Pin != nil {
		t.Fatalf("empty pin: {} must normalize to nil, got %+v", img.Pin)
	}
	if p := resolvedBox(t, empty).Pin; p != nil {
		t.Fatalf("absent pin must resolve to nil, got %+v", p)
	}

	// Absent-pin IR omits "pin"; pinned IR includes it.
	irNoPin, err := Desugar(empty, "flow")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EncodeIR(irNoPin)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"pin"`) {
		t.Fatalf("absent pin must be omitted from the IR:\n%s", b)
	}

	pinned, err := loadStr(t, pinNamedConfig(
		`  base: {packages: [git], pin: {rev: "25.11", sha256: "sha256:abc"}}`,
		`    image: base`))
	if err != nil {
		t.Fatal(err)
	}
	irPinned, err := Desugar(pinned, "flow")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := EncodeIR(irPinned)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pb), `"pin"`) {
		t.Fatalf("pinned IR must carry the pin field:\n%s", pb)
	}
}

// A partial pin (exactly one of rev/sha256 set) is a field-pathed completeness
// error naming ….pin — for both the named image and the inline build.
func TestPartialPinIsFieldPathedError(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantPath string
	}{
		{
			name:     "named image missing sha256",
			body:     pinNamedConfig(`  base: {packages: [git], pin: {rev: "25.11"}}`, `    image: base`),
			wantPath: "images.base.pin",
		},
		{
			name: "inline build missing rev",
			body: `
version: 1
templates:
  box:
    build: {packages: [git], pin: {sha256: "sha256:abc"}}
    skill: act
    inputs: {input: {type: string, required: true}}
    output: {result: {type: string, required: true}}
workflows:
  flow:
    params: {subject: {type: string, required: true}}
    steps: [{id: first, use: box, with: {input: "${params.subject}"}}]
`,
			wantPath: "templates.box.build.pin:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadStr(t, tc.body)
			if err == nil {
				t.Fatal("a partial pin must fail validation")
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Fatalf("error must name %q, got: %v", tc.wantPath, err)
			}
			if !strings.Contains(err.Error(), "rev and sha256 are both required") {
				t.Fatalf("error must explain completeness, got: %v", err)
			}
		})
	}
}

// A both-present pin with an off-charset rev/sha256 is a field-pathed violation
// at ….pin.rev / ….pin.sha256, collected (not first-error-only).
func TestOffCharsetPinIsFieldPathedError(t *testing.T) {
	_, err := loadStr(t, pinNamedConfig(
		`  base: {packages: [git], pin: {rev: "25.11; rm -rf", sha256: "bad$(x)"}}`,
		`    image: base`))
	if err == nil {
		t.Fatal("off-charset pin values must fail validation")
	}
	msg := err.Error()
	if !strings.Contains(msg, "images.base.pin.rev") {
		t.Fatalf("error must name images.base.pin.rev, got: %v", err)
	}
	if !strings.Contains(msg, "images.base.pin.sha256") {
		t.Fatalf("both off-charset fields must be collected, got: %v", err)
	}
}

// Both-absent and both-present valid pins pass.
func TestValidPinsPass(t *testing.T) {
	if _, err := loadStr(t, pinNamedConfig(`  base: {packages: [git]}`, `    image: base`)); err != nil {
		t.Fatalf("absent pin must validate: %v", err)
	}
	if _, err := loadStr(t, pinNamedConfig(
		`  base: {packages: [git], pin: {rev: "release-25.11", sha256: "sha256:1abc+/=._-"}}`,
		`    image: base`)); err != nil {
		t.Fatalf("valid both-present pin must validate: %v", err)
	}
}
