package contract

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/dmitriyb/faber/config"
)

// OutputSchema is the template's declared output fields — the config module's
// typing vocabulary, carried into the box as JSON via FABER_OUTPUT_SCHEMA.
type OutputSchema map[string]config.FieldDef

// ParseOutputSchema decodes the FABER_OUTPUT_SCHEMA value. Empty means no
// declared outputs.
func ParseOutputSchema(raw string) (OutputSchema, error) {
	if strings.TrimSpace(raw) == "" {
		return OutputSchema{}, nil
	}
	var s OutputSchema
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("contract: parse output schema: %w", err)
	}
	if s == nil {
		s = OutputSchema{}
	}
	return s, nil
}

// Violation is one output-schema violation. All violations of a payload are
// collected, never first-error.
type Violation struct {
	Field string
	Msg   string
}

func (v Violation) String() string { return v.Field + ": " + v.Msg }

// JoinViolations renders a collected violation list for a record's detail.
func JoinViolations(vs []Violation) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = v.String()
	}
	return strings.Join(parts, "; ")
}

// ValidateOutput checks a decoded payload against the declared output schema:
// every required field present, kinds exact (a JSON number satisfies an int
// field only when integral; no string/int coercion), enum membership
// respected. It returns all violations plus the sorted names of undeclared
// extra fields — extras are not violations; they stay in the record but are
// never threaded.
func ValidateOutput(schema OutputSchema, payload map[string]any) (violations []Violation, extras []string) {
	for _, name := range slices.Sorted(maps.Keys(schema)) {
		def := schema[name]
		val, ok := payload[name]
		if !ok {
			if def.Required {
				violations = append(violations, Violation{Field: name, Msg: "required field missing"})
			}
			continue
		}
		if msg := checkKind(def, val); msg != "" {
			violations = append(violations, Violation{Field: name, Msg: msg})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(payload)) {
		if _, ok := schema[name]; !ok {
			extras = append(extras, name)
		}
	}
	return violations, extras
}

// checkKind verifies one value against its declared type; "" means valid.
func checkKind(def config.FieldDef, val any) string {
	switch def.Type {
	case "string":
		s, ok := val.(string)
		if !ok {
			return fmt.Sprintf("expected string, got %s", jsonKind(val))
		}
		if len(def.Enum) > 0 && !slices.Contains(def.Enum, s) {
			return fmt.Sprintf("value %q not in enum [%s]", s, strings.Join(def.Enum, ", "))
		}
		return ""
	case "int":
		f, ok := val.(float64)
		if !ok || math.Trunc(f) != f || math.IsInf(f, 0) || math.IsNaN(f) {
			return fmt.Sprintf("expected int, got %s", jsonKind(val))
		}
		return ""
	case "bool":
		if _, ok := val.(bool); !ok {
			return fmt.Sprintf("expected bool, got %s", jsonKind(val))
		}
		return ""
	case "object":
		if _, ok := val.(map[string]any); !ok {
			return fmt.Sprintf("expected object, got %s", jsonKind(val))
		}
		return ""
	default:
		return fmt.Sprintf("undeclarable field type %q", def.Type)
	}
}

// jsonKind names a decoded JSON value's kind for violation messages.
func jsonKind(val any) string {
	switch v := val.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "bool"
	case float64:
		if math.Trunc(v) == v {
			return "number"
		}
		return "non-integral number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return fmt.Sprintf("%T", val)
	}
}
