package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// TypedValue is one param value carried with its declared type.
type TypedValue struct {
	Type  string `json:"type"`
	Value any    `json:"value"`
}

// Params is the typed param environment consumed by the Desugarer's binding
// descriptors and the executor.
type Params map[string]TypedValue

// CheckParams validates supplied --param k=v strings against a params
// declaration: type coercion from string form (int/bool parsed, object accepts
// JSON), enum membership, required presence, defaults applied. A missing
// required param is a hard error with no implicit fallback. All violations are
// joined, not first-error. Presence, not truthiness, is the contract: an empty
// string is an accepted value for a required string param.
func CheckParams(decl map[string]ParamDef, supplied map[string]string) (Params, error) {
	var errs []error
	out := Params{}
	declared := sortedKeys(decl)
	for _, name := range sortedKeys(supplied) {
		d, ok := decl[name]
		if !ok {
			errs = append(errs, &FieldError{
				Path: "params." + name,
				Msg:  fmt.Sprintf("unknown param (declared params: %s)", strings.Join(declared, ", ")),
			})
			continue
		}
		val, err := coerceParam(d, supplied[name])
		if err != nil {
			errs = append(errs, &FieldError{Path: "params." + name, Msg: err.Error()})
			continue
		}
		out[name] = TypedValue{Type: d.Type, Value: val}
	}
	for _, name := range declared {
		if _, ok := supplied[name]; ok {
			continue
		}
		d := decl[name]
		if d.Default != nil {
			def, err := normalizeValue(d.Default)
			if err != nil {
				errs = append(errs, &FieldError{Path: "params." + name, Msg: "invalid default: " + err.Error()})
				continue
			}
			out[name] = TypedValue{Type: d.Type, Value: def}
			continue
		}
		if d.Required {
			errs = append(errs, &FieldError{Path: "params." + name, Msg: "required param missing"})
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

func coerceParam(d ParamDef, raw string) (any, error) {
	switch d.Type {
	case "string":
		if len(d.Enum) > 0 && !contains(d.Enum, raw) {
			return nil, fmt.Errorf("value %q not in enum [%s]", raw, strings.Join(d.Enum, ", "))
		}
		return raw, nil
	case "int":
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("value %q is not an int", raw)
		}
		return n, nil
	case "bool":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("value %q is not a bool", raw)
		}
		return b, nil
	case "object":
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("value is not valid JSON: %v", err)
		}
		return v, nil
	default:
		return nil, fmt.Errorf("undeclarable type %q", d.Type)
	}
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

// normalizeValue converts a YAML-decoded value into a JSON-marshalable one
// (map keys must be strings) so IR emission stays deterministic and total.
func normalizeValue(v any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			n, err := normalizeValue(e)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("non-string map key %v", k)
			}
			n, err := normalizeValue(e)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			n, err := normalizeValue(e)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		return v, nil
	}
}

// yamlTypeName maps a YAML-decoded literal onto the params typing vocabulary.
func yamlTypeName(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case int, int64, uint64:
		return "int"
	case bool:
		return "bool"
	case float64, float32:
		return "float"
	case map[string]any, map[any]any, []any:
		return "object"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}
