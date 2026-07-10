package config

import (
	"fmt"
	"sort"
	"strconv"
)

// rewriteStepRefs scans CEL source for step references of the form
// steps.<id>.<field> (outside string literals), resolves each step id to an IR
// node id via resolve, and rewrites the reference to steps["<node-id>"].<field>
// — so the key a condition reads under is exactly the dep node id. It returns
// the rewritten source and the sorted, deduplicated dep node ids.
//
// This is the desugar-time extraction that lets the WiringChecker verify the
// references and the scheduler know a condition's dependencies. Compilation of
// the CEL itself happens at validate time (CompileCondition).
func rewriteStepRefs(src string, resolve func(step string) (string, error)) (string, []string, error) {
	var out []byte
	depSet := map[string]bool{}
	i := 0
	for i < len(src) {
		c := src[i]

		// Skip string literals: CEL "..." and '...' with backslash escapes.
		if c == '"' || c == '\'' {
			quote := c
			out = append(out, c)
			i++
			for i < len(src) {
				out = append(out, src[i])
				if src[i] == '\\' && i+1 < len(src) {
					out = append(out, src[i+1])
					i += 2
					continue
				}
				if src[i] == quote {
					i++
					break
				}
				i++
			}
			continue
		}

		if isIdentStart(c) && (i == 0 || !isIdentPart(src[i-1])) && (i == 0 || src[i-1] != '.') {
			word, end := readIdent(src, i)
			if word == "steps" && end < len(src) && src[end] == '.' {
				step, e2 := readIdent(src, end+1)
				if step == "" {
					return "", nil, fmt.Errorf("condition %q: malformed steps reference", src)
				}
				if e2 >= len(src) || src[e2] != '.' {
					return "", nil, fmt.Errorf("condition %q: step reference must be steps.%s.<field>", src, step)
				}
				field, e3 := readIdent(src, e2+1)
				if field == "" {
					return "", nil, fmt.Errorf("condition %q: step reference must be steps.%s.<field>", src, step)
				}
				nodeID, err := resolve(step)
				if err != nil {
					return "", nil, err
				}
				depSet[nodeID] = true
				out = append(out, []byte("steps["+strconv.Quote(nodeID)+"]."+field)...)
				i = e3
				continue
			}
			out = append(out, src[i:end]...)
			i = end
			continue
		}

		out = append(out, c)
		i++
	}
	deps := make([]string, 0, len(depSet))
	for d := range depSet {
		deps = append(deps, d)
	}
	sort.Strings(deps)
	return string(out), deps, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// readIdent reads an identifier starting at i; returns it and the index just
// past its end (empty identifier if src[i] is not an identifier start).
func readIdent(src string, i int) (string, int) {
	if i >= len(src) || !isIdentStart(src[i]) {
		return "", i
	}
	j := i
	for j < len(src) && isIdentPart(src[j]) {
		j++
	}
	return src[i:j], j
}
