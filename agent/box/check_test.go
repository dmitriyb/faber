package box

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
)

// writeContextDoc is the one bundle file a prelude owes.
func writeContextDoc(t *testing.T, d testDirs) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.bundle, contract.ContextDoc), []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Verifies c563d2055eb9: under FABER_CHECK=1 the agent phase is a no-op
// whatever the template declares, the hooks still run in order, outputs no
// hook wrote are placeholders that satisfy the schema, and the record says
// check beside ok.
func TestCheckRunSkipsAgentAndFillsOutputs(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	postlude := writeHook(t, d, contract.HookPostlude)
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == prelude {
			writeContextDoc(t, d)
		}
		return CmdResult{}, nil
	}
	schema := `{"pr": {"type": "int", "required": true}, "verdict": {"type": "string", "enum": ["approve", "changes"], "required": true}, "meta": {"type": "object"}, "done": {"type": "bool", "required": true}}`
	b := newTestBox(t, d, map[string]string{contract.EnvCheck: "1", contract.EnvOutputSchema: schema}, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var sawPostlude bool
	for _, call := range fr.calls {
		if call.Argv[0] == "agent-cli" {
			t.Fatalf("the agent ran in a check: %v", fr.argvs())
		}
		if call.Argv[0] == postlude {
			sawPostlude = true
		}
	}
	if !sawPostlude {
		t.Fatalf("the postlude must still run: %v", fr.argvs())
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusOK || !rec.Check || !rec.Fallback {
		t.Fatalf("record %+v, want ok, check, fallback", rec)
	}
	if rec.Payload["pr"] != float64(0) || rec.Payload["verdict"] != "approve" || rec.Payload["done"] != false {
		t.Errorf("placeholders: %+v", rec.Payload)
	}
	if _, ok := rec.Payload["meta"].(map[string]any); !ok {
		t.Errorf("object placeholder: %+v", rec.Payload["meta"])
	}
}

// Verifies c563d2055eb9: a hook that writes output.json in a check is held
// to the schema exactly as in a run — a wrong value fails the step, with
// the record still marked check — and a value other than "1" is an
// env-contract violation, never a guessed mode.
func TestCheckRunKeepsTheOutputContract(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == prelude {
			writeContextDoc(t, d)
			writeOutput(t, d, `{"verdict": "maybe"}`)
		}
		return CmdResult{}, nil
	}
	schema := `{"verdict": {"type": "string", "enum": ["approve", "changes"], "required": true}}`
	b := newTestBox(t, d, map[string]string{contract.EnvCheck: "1", contract.EnvOutputSchema: schema}, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusFailed || rec.Error.Reason != contract.ReasonOutputSchema || !rec.Check {
		t.Fatalf("record %+v, want failed output-schema, check", rec)
	}

	d2 := newTestDirs(t)
	b2 := newTestBox(t, d2, map[string]string{contract.EnvCheck: "yes"}, &fakeRunner{})
	if code := Main(context.Background(), b2); code != 1 {
		t.Fatalf("exit code = %d, want 1 for a malformed check flag", code)
	}
	if rec := readRecord(t, d2); rec.Error == nil || rec.Error.Reason != contract.ReasonEnvContract {
		t.Fatalf("record %+v, want env-contract", rec)
	}
}
