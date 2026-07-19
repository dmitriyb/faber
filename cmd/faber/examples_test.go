package main

// Keeps the shipped examples honest: every examples/*/orchestrator.yaml must
// pass `faber validate` (package proof unwired — it needs nix and is covered
// by the realinfra suite). A doc example that stops validating is a bug.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dmitriyb/faber/config"
)

func TestExamplesValidate(t *testing.T) {
	matches, err := filepath.Glob("../../examples/*/orchestrator.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no examples found; the glob or the layout moved")
	}
	for _, path := range matches {
		t.Run(filepath.Base(filepath.Dir(path)), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := config.RunWithDeps([]string{"validate", "--config", path}, &stdout, &stderr, config.Deps{})
			if code != 0 {
				t.Errorf("faber validate exited %d:\n%s", code, stderr.String())
			}
		})
	}
}

func TestExampleHooksExecutable(t *testing.T) {
	hooks, err := filepath.Glob("../../examples/*/hooks/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hooks {
		info, err := os.Stat(h)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", h)
		}
	}
}
