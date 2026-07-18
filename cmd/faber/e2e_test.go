package main

// End-to-end acceptance checks over the reference orchestrator.yaml (the
// spec's test_reference_workflows.md example, kept verbatim under
// testdata/reference). Everything here runs the real CLI dispatch in-process;
// only the nix package proof is unwired (it needs a real machine and is
// covered by infra's realinfra suite).

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
)

const referenceConfig = "testdata/reference/orchestrator.yaml"

func validateEmitIR(t *testing.T) []byte {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := config.RunWithDeps(
		[]string{"validate", "--config", referenceConfig, "--emit-ir"},
		&stdout, &stderr, config.Deps{})
	if code != 0 {
		t.Fatalf("faber validate exited %d: %s", code, stderr.String())
	}
	return stdout.Bytes()
}

// Verifies eb9f40119b1b, 0ebbdd8f836b, 3876114fb7a3 (acceptance scenario 5):
// `faber validate --emit-ir` on the reference file matches the committed
// golden byte-for-byte, across two independent runs.
func TestReferenceValidateEmitIRGolden(t *testing.T) {
	golden, err := os.ReadFile("testdata/reference/golden.ir.json")
	if err != nil {
		t.Fatal(err)
	}
	first := validateEmitIR(t)
	if !bytes.Equal(first, golden) {
		t.Fatalf("emitted IR differs from golden (regenerate with -tags goldenupdate if the change is intended)\ngot %d bytes, want %d", len(first), len(golden))
	}
	if second := validateEmitIR(t); !bytes.Equal(second, first) {
		t.Fatal("two validate --emit-ir runs disagree: IR emission is not deterministic")
	}
}

// Verifies eb9f40119b1b: the golden IR has the acceptance shape — the task
// loop unrolled into three conditional iterations plus the post-loop
// selector, and the epic generate node carrying the source and sub-workflow
// refs. Structural spot-checks so the byte-compare above cannot silently
// bless an empty or truncated golden.
func TestReferenceGoldenShape(t *testing.T) {
	ir := string(validateEmitIR(t))
	for _, want := range []string{
		`"workflow": "task"`,
		`"workflow": "epic"`,
		`"kind": "generate"`,
		`"kind": "selector"`,
		`"source": "members"`,
	} {
		if !strings.Contains(ir, want) {
			t.Errorf("emitted IR lacks %s", want)
		}
	}
	for _, iter := range []string{"review-cycle@1/review", "review-cycle@2/review", "review-cycle@3/review"} {
		if !strings.Contains(ir, iter) {
			t.Errorf("task loop not unrolled to iteration %s", iter)
		}
	}
	if strings.Contains(ir, `"kind": "loop"`) {
		t.Error("executed IR contains a loop node; unrolling failed")
	}
}

// Verifies 2d9d219e22c6 (leaf edge case): a with: binding typo'd to an
// undeclared output field fails faber validate with a field-path error —
// never the run.
func TestReferenceBrokenWiringFailsValidate(t *testing.T) {
	src, err := os.ReadFile(referenceConfig)
	if err != nil {
		t.Fatal(err)
	}
	broken := bytes.ReplaceAll(src, []byte("${steps.implement.pr}"), []byte("${steps.implement.prs}"))
	if bytes.Equal(broken, src) {
		t.Fatal("fixture edit did not apply")
	}
	dir := t.TempDir()
	path := dir + "/orchestrator.yaml"
	if err := os.WriteFile(path, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := config.RunWithDeps([]string{"validate", "--config", path}, &stdout, &stderr, config.Deps{})
	if code == 0 {
		t.Fatal("validate accepted a reference to an undeclared output field")
	}
	if out := stderr.String(); !strings.Contains(out, "prs") || !strings.Contains(out, "implement") {
		t.Errorf("error does not carry the field path to the bad reference:\n%s", out)
	}
}

// Verifies 67c77533453d: the shipped binary's dependency injection is
// complete — every seam non-nil — and the executor adapter refuses to run
// without the CLI wiring context instead of panicking on nil config.
func TestWiredDepsComplete(t *testing.T) {
	deps := wireDeps(os.Stdout, os.Stderr)
	if deps.Prover == nil || deps.Builder == nil || deps.Executor == nil || deps.Journal == nil {
		t.Fatalf("wireDeps left a seam nil: %+v", deps)
	}
	err := deps.Executor.Execute(t.Context(), &config.IR{}, nil, config.RunOptions{}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "wiring context") {
		t.Errorf("executor without wiring context: want a clear refusal, got %v", err)
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
