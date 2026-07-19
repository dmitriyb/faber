//go:build goldenupdate

package config

// Regenerates the reference golden IRs:
//
//	go test -tags goldenupdate -run TestUpdateReferenceGoldens ./config
//
// Each golden is the exact stdout of `faber validate --emit-ir` on the
// reference config with the package proof unwired (the proof needs nix; IR
// emission must not).

import (
	"bytes"
	"os"
	"testing"
)

func TestUpdateReferenceGoldens(t *testing.T) {
	for workflow, golden := range map[string]string{
		"task": "testdata/reference_task.ir.json",
		"epic": "testdata/reference_epic.ir.json",
	} {
		var stdout, stderr bytes.Buffer
		code := RunWithDeps(
			[]string{"validate", "--config", "testdata/reference.yaml", "--workflow", workflow, "--emit-ir"},
			&stdout, &stderr, Deps{})
		if code != 0 {
			t.Fatalf("validate %s exited %d: %s", workflow, code, stderr.String())
		}
		if err := os.WriteFile(golden, stdout.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
