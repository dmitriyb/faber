package metering

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Tier names. Fidelity ordering is advice, not enforcement: exact where you
// can, reported where the vendor allows, probe as a last resort — and beneath
// all three, the rate-limit floor works with zero configuration.
const (
	TierExact    = "exact"
	TierReported = "reported"
	TierProbe    = "probe"
)

// Config is the run-entry metering config file: measurement policy is run
// policy, separate from workflow structure (orchestrator.yaml is untouched),
// which is what lets the same workflow run metered in one environment and
// unmetered in another. No file and no --budget flags means the no-op
// default: an empty meter set.
type Config struct {
	Endpoints map[string]ClassDef `yaml:"endpoints"`
}

// ClassDef declares one endpoint class — a user-chosen name for an API
// surface — mapping it to a tier, a unit, tier-specific settings, and the
// templates it covers (default: all).
type ClassDef struct {
	Tier      string            `yaml:"tier"`
	Unit      string            `yaml:"unit,omitempty"`
	Fields    map[string]string `yaml:"fields,omitempty"`    // sidecar field -> unit (reported); optional allowlist into the tier's unit (exact)
	Templates []string          `yaml:"templates,omitempty"` // empty: covers all templates
	Tokenizer *TokenizerDef     `yaml:"tokenizer,omitempty"` // exact
	Probe     *ProbeDef         `yaml:"probe,omitempty"`     // probe
}

// TokenizerDef configures the exact tier's opaque tokenizer command.
type TokenizerDef struct {
	Command   Argv  `yaml:"command"`
	MaxOutput int64 `yaml:"max_output"`
}

// ProbeDef configures the probe tier's opaque saturation probe. Threshold
// defaults to 1.0 (defer only on full saturation) when omitted.
type ProbeDef struct {
	Command   Argv     `yaml:"command"`
	Threshold *float64 `yaml:"threshold,omitempty"`
}

// Argv is an explicit command argument list. YAML may spell it as a single
// scalar (the command path) or a sequence (path plus arguments); either way
// execution uses the explicit list — never a shell.
type Argv []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (a *Argv) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		*a = Argv{s}
		return nil
	case yaml.SequenceNode:
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		*a = Argv(list)
		return nil
	default:
		return fmt.Errorf("command must be a string or a list of strings")
	}
}

// LoadConfig reads, parses, and validates a metering config file. templates
// is the orchestrator config's template names for resolving each class's
// coverage; nil skips that resolution (structural checks only). Probe and
// tokenizer commands are opaque user paths — faber validates presence, never
// content.
func LoadConfig(path string, templates []string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("metering: read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("metering: parse %s: %w", path, err)
	}
	if err := validateConfig(&cfg, templates); err != nil {
		return nil, fmt.Errorf("metering: %s: %w", path, err)
	}
	return &cfg, nil
}

// validateConfig checks tier names, tier-required keys, template resolution,
// and the one-class-per-template rule, reporting all findings at once.
func validateConfig(cfg *Config, templates []string) error {
	var errs []error
	check := func(cond bool, path, msg string) {
		if !cond {
			errs = append(errs, fmt.Errorf("%s: %s", path, msg))
		}
	}
	known := map[string]bool{}
	for _, t := range templates {
		known[t] = true
	}

	classes := make([]string, 0, len(cfg.Endpoints))
	for name := range cfg.Endpoints {
		classes = append(classes, name)
	}
	sort.Strings(classes)

	claimed := map[string]string{} // template -> claiming class
	catchAll := ""
	for _, name := range classes {
		class := cfg.Endpoints[name]
		path := "endpoints." + name
		switch class.Tier {
		case TierExact:
			check(class.Unit != "", path+".unit", "required for the exact tier")
			if class.Tokenizer == nil {
				check(false, path+".tokenizer", "required for the exact tier")
			} else {
				check(len(class.Tokenizer.Command) > 0, path+".tokenizer.command", "required")
				check(class.Tokenizer.MaxOutput >= 0, path+".tokenizer.max_output", "must not be negative")
			}
			// Optional usage allowlist: which sidecar fields settle as this
			// tier's unit. The exact tier reports one unit, so every mapping
			// must name it — anything else would coerce a foreign unit into
			// the bound's unit.
			for _, f := range sortedFieldNames(class.Fields) {
				check(class.Fields[f] == class.Unit, path+".fields."+f, "exact tier usage fields must map to the tier's unit")
			}
		case TierReported:
			check(class.Unit != "", path+".unit", "required for the reported tier")
			check(len(class.Fields) > 0, path+".fields", "required for the reported tier")
			for _, f := range sortedFieldNames(class.Fields) {
				check(class.Fields[f] != "", path+".fields."+f, "unit must not be empty")
			}
		case TierProbe:
			if class.Probe == nil {
				check(false, path+".probe", "required for the probe tier")
			} else {
				check(len(class.Probe.Command) > 0, path+".probe.command", "required")
				if class.Probe.Threshold != nil {
					t := *class.Probe.Threshold
					check(t >= 0 && t <= 1, path+".probe.threshold", "must be within [0, 1]")
				}
			}
		default:
			check(false, path+".tier", fmt.Sprintf("unknown tier %q (exact|reported|probe)", class.Tier))
		}

		if len(class.Templates) == 0 {
			if catchAll != "" {
				check(false, path+".templates", fmt.Sprintf("classes %q and %q both cover all templates; a template must map to one class", catchAll, name))
			} else {
				catchAll = name
			}
			continue
		}
		for _, t := range class.Templates {
			if prev, dup := claimed[t]; dup {
				check(false, path+".templates", fmt.Sprintf("template %q already covered by class %q; a template must map to one class", t, prev))
				continue
			}
			claimed[t] = name
			if templates != nil {
				check(known[t], path+".templates", fmt.Sprintf("unknown template %q", t))
			}
		}
	}
	return errors.Join(errs...)
}

// ClassForTemplate resolves the endpoint class covering a template: an
// explicit templates listing wins over a catch-all class; no match yields ""
// (the template runs unmetered).
func (c *Config) ClassForTemplate(template string) string {
	if c == nil {
		return ""
	}
	catchAll := ""
	for _, name := range sortedClassNames(c.Endpoints) {
		class := c.Endpoints[name]
		if len(class.Templates) == 0 {
			catchAll = name
			continue
		}
		for _, t := range class.Templates {
			if t == template {
				return name
			}
		}
	}
	return catchAll
}

// BuildMeters constructs the per-endpoint-class meter sets from a validated
// config. A nil config yields the empty meter set (the no-op default). A nil
// runner uses the production ExecRunner; a nil logger discards.
func BuildMeters(cfg *Config, runner ProbeRunner, logger *slog.Logger) map[string][]Meter {
	if cfg == nil || len(cfg.Endpoints) == 0 {
		return nil
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	meters := make(map[string][]Meter, len(cfg.Endpoints))
	for _, name := range sortedClassNames(cfg.Endpoints) {
		class := cfg.Endpoints[name]
		log := logger.With("endpoint", name)
		switch class.Tier {
		case TierExact:
			meters[name] = append(meters[name],
				newExactMeter(Unit(class.Unit), class.Tokenizer.Command, class.Tokenizer.MaxOutput,
					sortedFieldNames(class.Fields), runner, log))
		case TierReported:
			fields := make(map[string]Unit, len(class.Fields))
			for f, u := range class.Fields {
				fields[f] = Unit(u)
			}
			meters[name] = append(meters[name], newReportedMeter(Unit(class.Unit), fields, log))
		case TierProbe:
			threshold := 1.0
			if class.Probe.Threshold != nil {
				threshold = *class.Probe.Threshold
			}
			meters[name] = append(meters[name], newProbeMeter(class.Probe.Command, threshold, runner, log))
		}
	}
	return meters
}

func sortedClassNames(endpoints map[string]ClassDef) []string {
	names := make([]string, 0, len(endpoints))
	for name := range endpoints {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedFieldNames(fields map[string]string) []string {
	if len(fields) == 0 {
		return nil
	}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
