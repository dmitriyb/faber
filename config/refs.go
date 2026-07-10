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
// in v1; concatenation belongs in hooks.
func ParseBinding(v any) (Binding, error) {
	s, ok := v.(string)
	if !ok || !strings.Contains(s, "${") {
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
