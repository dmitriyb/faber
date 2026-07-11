package pipeline

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/dmitriyb/faber/config"
)

// resolveBinding resolves one non-edge binding descriptor against a parameter
// scope. ok is false when a param binding names a param the scope does not
// carry (legal for optional params; the slot's declared default applies).
func resolveBinding(bd config.BindingDesc, env *scopeEnv) (any, bool, error) {
	switch bd.Kind {
	case config.BindLiteral:
		return bd.Value, true, nil
	case config.BindParam:
		v, ok := env.params[bd.Name]
		return v, ok, nil
	case config.BindItem:
		// Item bindings live only inside a generate's binding template and are
		// resolved to values at expansion; one surviving to a node is a bug.
		return nil, false, fmt.Errorf("item binding %q survived to run time", bd.Field)
	default:
		return nil, false, fmt.Errorf("unknown binding kind %q", bd.Kind)
	}
}

// resolveInputs resolves an agent node's input slots: binding descriptors,
// inbound data edges (the source's settled payload field), then declared slot
// defaults for anything still unbound. skipped names the first data-edge
// source that settled skipped — the value cannot exist, so the node settles
// skipped-dependency naming it.
func resolveInputs(g *graph, n *execNode) (inputs map[string]any, skipped string, err error) {
	inputs = map[string]any{}
	for _, slot := range sortedKeys(n.n.Bindings) {
		v, ok, err := resolveBinding(n.n.Bindings[slot], n.env)
		if err != nil {
			return nil, "", fmt.Errorf("input %q: %w", slot, err)
		}
		if ok {
			inputs[slot] = v
		}
	}
	for _, e := range g.dataIn[n.n.ID] {
		src, ok := g.nodes[e.From]
		if !ok || !src.terminal() {
			return nil, "", fmt.Errorf("input %q: source %s has not settled", e.ToPort, e.From)
		}
		switch src.status {
		case StateOK:
			v, ok := src.payload[e.FromPort]
			if !ok {
				return nil, "", fmt.Errorf("input %q: source %s settled without field %q", e.ToPort, e.From, e.FromPort)
			}
			inputs[e.ToPort] = v
		case StateSkippedCondition, StateSkippedDependency:
			return nil, e.From, nil
		default:
			// A failed source never releases this node: propagation settles it
			// first. Reaching here is a scheduler bug.
			return nil, "", fmt.Errorf("input %q: source %s settled %s", e.ToPort, e.From, src.status)
		}
	}
	if n.n.Template != nil {
		for _, slot := range sortedKeys(n.n.Template.Inputs) {
			if _, bound := inputs[slot]; bound {
				continue
			}
			if def := n.n.Template.Inputs[slot].Default; def != nil {
				inputs[slot] = def
			}
		}
	}
	return inputs, "", nil
}

// scalarString renders a resolved value for command args and box input env:
// scalars verbatim, compounds as JSON. It mirrors the failure module's hook
// env rendering so the same value reads identically everywhere.
func scalarString(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case json.Number:
		return t.String(), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", fmt.Errorf("render value: %w", err)
		}
		return string(b), nil
	}
}

// paramValues flattens the typed run params into the executor's root scope.
func paramValues(params config.Params) map[string]any {
	out := make(map[string]any, len(params))
	for name, tv := range params {
		out[name] = tv.Value
	}
	return out
}

// decodePayload decodes a settled ok payload for threading and conditions.
func decodePayload(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}
