package metering

import (
	"path/filepath"
	"strings"
	"testing"
)

var testTemplates = []string{"step-a", "step-b", "step-c"}

func load(t *testing.T, name string, templates []string) *Config {
	t.Helper()
	cfg, err := LoadConfig(filepath.Join("testdata", name), templates)
	if err != nil {
		t.Fatalf("LoadConfig(%s): %v", name, err)
	}
	return cfg
}

// Verifies e3dae1b52167: tier assignment is declared per endpoint class in a
// run-entry metering config file — one fixture per tier plus a multi-class
// file all load and carry their tier-specific settings.
func TestLoadConfigTierFixtures(t *testing.T) {
	exact := load(t, "meters_exact.yaml", testTemplates)
	class := exact.Endpoints["endpoint-a"]
	if class.Tier != TierExact || class.Unit != "tokens" ||
		len(class.Tokenizer.Command) != 1 || class.Tokenizer.Command[0] != "./hooks/tokenize" ||
		class.Tokenizer.MaxOutput != 8192 {
		t.Fatalf("exact fixture decoded as %+v", class)
	}
	if class.Fields["prompt_tokens"] != "tokens" || class.Fields["completion_tokens"] != "tokens" {
		t.Fatalf("exact fixture usage allowlist decoded as %v", class.Fields)
	}

	reported := load(t, "meters_reported.yaml", testTemplates)
	class = reported.Endpoints["endpoint-a"]
	if class.Tier != TierReported || class.Fields["input_tokens"] != "tokens" {
		t.Fatalf("reported fixture decoded as %+v", class)
	}

	probe := load(t, "meters_probe.yaml", testTemplates)
	class = probe.Endpoints["endpoint-a"]
	if class.Tier != TierProbe || len(class.Probe.Command) != 2 || *class.Probe.Threshold != 0.9 {
		t.Fatalf("probe fixture decoded as %+v", class)
	}

	if _, err := LoadConfig(filepath.Join("testdata", "meters_multi.yaml"), testTemplates); err != nil {
		t.Fatalf("multi-class fixture: %v", err)
	}
}

// Verifies e3dae1b52167: template coverage resolves each template to exactly
// one endpoint class — an explicit listing wins over the catch-all, and a
// template no class covers runs unmetered.
func TestClassForTemplate(t *testing.T) {
	multi := load(t, "meters_multi.yaml", testTemplates)
	if got := multi.ClassForTemplate("step-a"); got != "endpoint-a" {
		t.Fatalf("step-a -> %q, want the explicit class endpoint-a", got)
	}
	if got := multi.ClassForTemplate("step-b"); got != "endpoint-b" {
		t.Fatalf("step-b -> %q, want the catch-all endpoint-b", got)
	}

	reported := load(t, "meters_reported.yaml", testTemplates)
	if got := reported.ClassForTemplate("step-c"); got != "" {
		t.Fatalf("uncovered template -> %q, want unmetered", got)
	}
	var nilCfg *Config
	if got := nilCfg.ClassForTemplate("step-a"); got != "" {
		t.Fatalf("nil config -> %q, want unmetered", got)
	}
}

// Verifies e3dae1b52167: load-time validation — known tier names,
// tier-required keys, template resolution, one class per template, strict
// keys — each invalid fixture is rejected with a diagnosable error.
func TestLoadConfigRejectsInvalid(t *testing.T) {
	cases := []struct {
		fixture string
		wantErr string
	}{
		{"meters_invalid_tier.yaml", "unknown tier"},
		{"meters_invalid_exact_no_tokenizer.yaml", "tokenizer: required"},
		{"meters_invalid_exact_fields_unit.yaml", "must map to the tier's unit"},
		{"meters_invalid_reported_no_fields.yaml", "fields: required"},
		{"meters_invalid_probe_no_command.yaml", "probe.command: required"},
		{"meters_invalid_no_unit.yaml", "unit: required"},
		{"meters_invalid_dup_template.yaml", "must map to one class"},
		{"meters_invalid_two_catchall.yaml", "both cover all templates"},
		{"meters_invalid_threshold.yaml", "within [0, 1]"},
		{"meters_invalid_unknown_key.yaml", "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			_, err := LoadConfig(filepath.Join("testdata", tc.fixture), testTemplates)
			if err == nil {
				t.Fatalf("LoadConfig(%s) succeeded, want error containing %q", tc.fixture, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}

	// A template listed in the file but absent from the orchestrator config.
	if _, err := LoadConfig(filepath.Join("testdata", "meters_reported.yaml"), []string{"step-x"}); err == nil ||
		!strings.Contains(err.Error(), "unknown template") {
		t.Fatalf("unknown template accepted: %v", err)
	}
	// nil template list skips resolution entirely.
	if _, err := LoadConfig(filepath.Join("testdata", "meters_reported.yaml"), nil); err != nil {
		t.Fatalf("nil template list must skip resolution: %v", err)
	}
}

// Verifies e3dae1b52167: BuildMeters constructs one reference implementation
// per class behind the same Meter interface, and a nil config yields the
// empty meter set (the no-op default).
func TestBuildMeters(t *testing.T) {
	if got := BuildMeters(nil, nil, nil); got != nil {
		t.Fatalf("BuildMeters(nil) = %v, want the empty meter set", got)
	}

	multi := load(t, "meters_multi.yaml", testTemplates)
	meters := BuildMeters(multi, &fakeRunner{}, nil)
	if len(meters) != 2 || len(meters["endpoint-a"]) != 1 || len(meters["endpoint-b"]) != 1 {
		t.Fatalf("BuildMeters(multi) = %v, want one meter per class", meters)
	}
	if _, ok := meters["endpoint-a"][0].(*exactMeter); !ok {
		t.Fatalf("endpoint-a meter is %T, want the exact tier", meters["endpoint-a"][0])
	}
	if _, ok := meters["endpoint-b"][0].(*reportedMeter); !ok {
		t.Fatalf("endpoint-b meter is %T, want the reported tier", meters["endpoint-b"][0])
	}

	probe := load(t, "meters_probe.yaml", testTemplates)
	pm, ok := BuildMeters(probe, &fakeRunner{}, nil)["endpoint-a"][0].(*probeMeter)
	if !ok || pm.threshold != 0.9 {
		t.Fatalf("probe class built as %#v, want probe tier with threshold 0.9", pm)
	}
}
