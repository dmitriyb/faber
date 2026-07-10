package config

import (
	"fmt"
	"strings"
)

// RefRoot is one of the four legal interpolation roots. Nothing else
// interpolates: no environment access, no host filesystem access, no arbitrary
// expression in a binding.
type RefRoot string

// The closed set of reference roots.
const (
	RootParams  RefRoot = "params"
	RootSteps   RefRoot = "steps"
	RootItem    RefRoot = "item"
	RootSources RefRoot = "sources"
)

// Ref is one parsed ${...} reference. For RootSteps, Name is the step id and
// Field the output field; for the other roots Name carries the single segment
// and Field is empty.
type Ref struct {
	Root  RefRoot
	Name  string
	Field string
}

func (r Ref) String() string {
	if r.Field != "" {
		return string(r.Root) + "." + r.Name + "." + r.Field
	}
	return string(r.Root) + "." + r.Name
}

// Binding classifies a with: value as either a literal or exactly one
// interpolated reference.
type Binding struct {
	IsRef   bool
	Ref     Ref
	Literal any
}

// ParseBinding classifies a with: value. A string containing "${" must be
// exactly one ${ref} with no surrounding text — there is no string templating
// in v1; concatenation belongs in hooks. Interpolation is legal only as a
// whole scalar value: a "${" nested inside a compound (map/list) literal is
// rejected rather than silently passed through as literal text.
func ParseBinding(v any) (Binding, error) {
	s, ok := v.(string)
	if !ok {
		if err := rejectNestedRefs(v); err != nil {
			return Binding{}, err
		}
		return Binding{Literal: v}, nil
	}
	if !strings.Contains(s, "${") {
		return Binding{Literal: v}, nil
	}
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") ||
		strings.Count(s, "${") != 1 || strings.Count(s, "}") != 1 {
		return Binding{}, fmt.Errorf("value %q: interpolation must be exactly one ${ref} with no surrounding text", s)
	}
	ref, err := ParseRef(s[2 : len(s)-1])
	if err != nil {
		return Binding{}, err
	}
	return Binding{IsRef: true, Ref: ref}, nil
}

// rejectNestedRefs walks a compound literal value and rejects any nested
// string containing "${": references do not interpolate inside object or list
// literals, and silently treating one as literal text would mask the typo.
func rejectNestedRefs(v any) error {
	switch t := v.(type) {
	case string:
		if strings.Contains(t, "${") {
			return fmt.Errorf("value %q: ${...} references are only legal as a whole scalar binding value, not nested inside a map or list literal", t)
		}
	case map[string]any:
		for _, k := range sortedKeys(t) {
			if err := rejectNestedRefs(t[k]); err != nil {
				return err
			}
		}
	case map[any]any:
		for _, e := range t {
			if err := rejectNestedRefs(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := rejectNestedRefs(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// ParseRef parses the inside of a ${...} reference. It rejects unknown roots,
// empty segments, and steps refs without an output field.
func ParseRef(s string) (Ref, error) {
	parts := strings.Split(s, ".")
	for _, p := range parts {
		if p == "" {
			return Ref{}, fmt.Errorf("reference %q: empty segment", s)
		}
	}
	switch RefRoot(parts[0]) {
	case RootParams, RootItem, RootSources:
		if len(parts) != 2 {
			return Ref{}, fmt.Errorf("reference %q: %s references take exactly one segment (%s.name)", s, parts[0], parts[0])
		}
		return Ref{Root: RefRoot(parts[0]), Name: parts[1]}, nil
	case RootSteps:
		if len(parts) != 3 {
			return Ref{}, fmt.Errorf("reference %q: steps references must be steps.<id>.<field>", s)
		}
		return Ref{Root: RootSteps, Name: parts[1], Field: parts[2]}, nil
	default:
		return Ref{}, fmt.Errorf("reference %q: unknown root %q (legal roots: params, steps, item, sources)", s, parts[0])
	}
}
