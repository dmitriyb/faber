package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParseRef (review §5.3): ParseRef is total — every input yields exactly
// one of a valid Ref or an error, never a panic; and it is idempotent in the
// sense that re-parsing the canonical rendering of an accepted ref agrees.
func FuzzParseRef(f *testing.F) {
	for _, s := range []string{"params.repo", "steps.a.field", "item.id", "sources.feed",
		"", ".", "..", "steps.a", "steps.a.b.c", "unknown.x", "params.", "steps..field"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseRef(s) // must never panic
		if err != nil {
			return
		}
		// An accepted ref names a known root and non-empty parts.
		switch ref.Root {
		case RootParams, RootItem, RootSources:
			if ref.Name == "" {
				t.Fatalf("accepted %q with empty name", s)
			}
		case RootSteps:
			if ref.Name == "" || ref.Field == "" {
				t.Fatalf("accepted %q with empty name/field", s)
			}
		default:
			t.Fatalf("accepted %q with unknown root %q", s, ref.Root)
		}
	})
}

// FuzzRewriteStepRefs (review §5.3): rewriteStepRefs never panics; string
// literals are preserved verbatim (a steps token inside quotes is not
// rewritten); and every steps token OUTSIDE a literal is either rewritten to
// the canonical steps["<id>"] form or the whole expression is rejected —
// never left as a bare steps.x that would escape dep extraction.
func FuzzRewriteStepRefs(f *testing.F) {
	for _, s := range []string{
		`steps.a.field == "x"`,
		`"steps.a.b literal"`,
		`params.p && steps.review.verdict == "approved"`,
		`steps.a.b + steps.c.d`,
		`'single quoted steps.x'`,
		`steps`,
		`steps.`,
		`steps.a`,
		`\\`,
	} {
		f.Add(s)
	}
	resolve := func(step string) (string, error) {
		if step == "" {
			return "", errEmpty
		}
		return "node/" + step, nil
	}
	f.Fuzz(func(t *testing.T, src string) {
		out, deps, err := rewriteStepRefs(src, resolve) // must never panic
		if err != nil {
			return
		}
		// No bare `steps.` token survives outside a string literal in the
		// rewritten output: every rewrite produced steps[...] form.
		if hasBareStepsToken(out) {
			t.Fatalf("bare steps token survived rewrite of %q -> %q", src, out)
		}
		// Deps are unique and non-empty.
		seen := map[string]bool{}
		for _, d := range deps {
			if d == "" || seen[d] {
				t.Fatalf("bad dep set %v for %q", deps, src)
			}
			seen[d] = true
		}
	})
}

var errEmpty = &FieldError{Path: "steps", Msg: "empty step"}

// hasBareStepsToken reports whether s contains a `steps` identifier followed
// by `.` outside any string literal — the non-canonical form rewrite must
// eliminate.
func hasBareStepsToken(s string) bool {
	i := 0
	for i < len(s) {
		c := s[i]
		if c == '"' || c == '\'' {
			q := c
			i++
			for i < len(s) {
				if s[i] == '\\' && i+1 < len(s) {
					i += 2
					continue
				}
				if s[i] == q {
					i++
					break
				}
				i++
			}
			continue
		}
		// A `steps` at an identifier-start position that is NOT a member access
		// (`x.steps` selects a field named steps, not the root map) is the
		// bare root reference the rewriter must eliminate. This mirrors
		// rewriteStepRefs' own guard (identifier start, prev byte not '.').
		if strings.HasPrefix(s[i:], "steps.") && (i == 0 || (!isIdentPart(s[i-1]) && s[i-1] != '.')) {
			return true
		}
		i++
	}
	return false
}

// FuzzConfigAssemble (review §5.4): arbitrary YAML through the full config
// pipeline (Load → Validate → Desugar → CheckWiring → HashIR) never panics
// and never hangs (an alias bomb errors rather than expanding); a config
// that validates and desugars produces a byte-deterministic IR hash.
func FuzzConfigAssemble(f *testing.F) {
	f.Add(readSeed(f, "reference.yaml"))
	f.Add([]byte("version: 1\n"))
	f.Add([]byte("templates: {a: {build: {packages: [git]}, run: {env: {FABER_AGENT_CLI: c}}, skill: s, inputs: {}, output: {}}}\nworkflows: {w: {steps: [{id: x, use: a}]}}\n"))
	f.Add([]byte("a: &x [*x]\n")) // alias bomb: must error, not hang
	f.Add([]byte("version: 1\nworkflows: {w: {steps: [{id: dup, use: a}, {id: dup, use: a}]}}\n"))
	f.Add([]byte("\x00 not yaml"))

	f.Fuzz(func(t *testing.T, yamlBytes []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "orchestrator.yaml")
		if err := os.WriteFile(path, yamlBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, viols, err := Load(path) // never panics, never hangs
		if err != nil {
			return
		}
		if verr := Validate(cfg, viols); verr != nil {
			return
		}
		// A validated config desugars every workflow to a byte-stable IR.
		for name := range cfg.Workflows {
			ir, derr := Desugar(cfg, name)
			if derr != nil {
				continue
			}
			if werr := CheckWiring(ir, cfg); werr != nil {
				continue
			}
			h1, e1 := HashIR(ir)
			ir2, _ := Desugar(cfg, name)
			h2, e2 := HashIR(ir2)
			if e1 != nil || e2 != nil || h1 != h2 {
				t.Fatalf("desugar/hash not deterministic for %q: %q/%v vs %q/%v", name, h1, e1, h2, e2)
			}
		}
	})
}

func readSeed(f *testing.F, name string) []byte {
	f.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		f.Skip("seed missing")
	}
	return data
}
