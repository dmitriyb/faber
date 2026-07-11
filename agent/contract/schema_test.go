package contract

import (
	"fmt"
	"testing"

	"github.com/dmitriyb/faber/config"
)

// Verifies ff8e85704b0a: output validation checks required presence, exact
// kinds with no coercion, and enum membership — and collects all violations
// instead of stopping at the first.
func TestValidateOutput(t *testing.T) {
	schema := OutputSchema{
		"verdict": {Type: "string", Required: true, Enum: []string{"ok", "changes"}},
		"count":   {Type: "int", Required: true},
		"flag":    {Type: "bool"},
		"meta":    {Type: "object"},
	}
	tests := []struct {
		name       string
		payload    map[string]any
		wantFields []string // violated fields, sorted
		wantExtras []string
	}{
		{
			name:    "valid payload",
			payload: map[string]any{"verdict": "ok", "count": float64(2), "flag": true, "meta": map[string]any{"k": "v"}},
		},
		{
			name:       "required fields missing",
			payload:    map[string]any{},
			wantFields: []string{"count", "verdict"},
		},
		{
			name:       "no string-int coercion either way",
			payload:    map[string]any{"verdict": "ok", "count": "2"},
			wantFields: []string{"count"},
		},
		{
			name:       "non-integral number is not an int",
			payload:    map[string]any{"verdict": "ok", "count": 2.5},
			wantFields: []string{"count"},
		},
		{
			name:       "enum membership",
			payload:    map[string]any{"verdict": "maybe", "count": float64(1)},
			wantFields: []string{"verdict"},
		},
		{
			name:       "all violations collected",
			payload:    map[string]any{"verdict": "maybe", "count": true, "flag": "yes"},
			wantFields: []string{"count", "flag", "verdict"},
		},
		{
			name:       "extras are not violations",
			payload:    map[string]any{"verdict": "ok", "count": float64(1), "surplus": "x", "more": float64(2)},
			wantExtras: []string{"more", "surplus"},
		},
		{
			name:       "null is no kind",
			payload:    map[string]any{"verdict": nil, "count": float64(1)},
			wantFields: []string{"verdict"},
		},
		{
			name:       "array is not object",
			payload:    map[string]any{"verdict": "ok", "count": float64(1), "meta": []any{"a"}},
			wantFields: []string{"meta"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations, extras := ValidateOutput(schema, tt.payload)
			var fields []string
			for _, v := range violations {
				fields = append(fields, v.Field)
			}
			if fmt.Sprint(fields) != fmt.Sprint(tt.wantFields) {
				t.Fatalf("violated fields = %v, want %v (%s)", fields, tt.wantFields, JoinViolations(violations))
			}
			if fmt.Sprint(extras) != fmt.Sprint(tt.wantExtras) {
				t.Fatalf("extras = %v, want %v", extras, tt.wantExtras)
			}
		})
	}
}

// Verifies ff8e85704b0a: an undeclarable field type in the schema is itself
// a violation when a value arrives under it.
func TestValidateOutputUndeclarableType(t *testing.T) {
	violations, _ := ValidateOutput(OutputSchema{"x": {Type: "float"}}, map[string]any{"x": 1.5})
	if len(violations) != 1 {
		t.Fatalf("violations = %v", violations)
	}
}

// Verifies ff8e85704b0a: the schema travels the env contract as JSON; empty
// means no declared outputs.
func TestParseOutputSchema(t *testing.T) {
	s, err := ParseOutputSchema(`{"verdict": {"type": "string", "required": true, "enum": ["ok"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	want := config.FieldDef{Type: "string", Required: true, Enum: []string{"ok"}}
	if fmt.Sprint(s["verdict"]) != fmt.Sprint(want) {
		t.Fatalf("schema = %+v", s)
	}
	if s, err := ParseOutputSchema(""); err != nil || len(s) != 0 {
		t.Fatalf("empty schema: %v %v", s, err)
	}
	if _, err := ParseOutputSchema("{"); err == nil {
		t.Fatal("malformed schema JSON must error")
	}
}
