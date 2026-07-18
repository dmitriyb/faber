//go:build goldenupdate

package main

// Regenerates the reference golden IR:
//
//	go test -tags goldenupdate -run TestUpdateReferenceGolden ./cmd/faber
//
// The golden is the exact stdout of `faber validate --emit-ir` on the
// reference orchestrator.yaml with the package proof unwired (the proof needs
// nix; IR emission must not).

import (
	"bytes"
	"os"
	"testing"

	"github.com/dmitriyb/faber/config"
)

func TestUpdateReferenceGolden(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := config.RunWithDeps(
		[]string{"validate", "--config", "testdata/reference/orchestrator.yaml", "--emit-ir"},
		&stdout, &stderr, config.Deps{})
	if code != 0 {
		t.Fatalf("validate exited %d: %s", code, stderr.String())
	}
	if err := os.WriteFile("testdata/reference/golden.ir.json", stdout.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
