package config

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

// fakeRegistry records add-key/list-keys calls and returns a scripted error so
// the CLI's exit-code mapping is testable without the security module.
type fakeRegistry struct {
	added   []string // "role fingerprint comment force"
	addErr  error
	listErr error
	listOut string
}

func (f *fakeRegistry) AddKey(role, fingerprint, comment string, force bool) error {
	f.added = append(f.added, fmt.Sprintf("%s %s %s %v", role, fingerprint, comment, force))
	return f.addErr
}

func (f *fakeRegistry) ListKeys(stdout, stderr io.Writer) error {
	if f.listErr != nil {
		return f.listErr
	}
	fmt.Fprint(stdout, f.listOut)
	return nil
}

const goodFP = "SHA256:abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"

// Verifies CLI commands (add-key): a well-formed invocation dispatches to the
// controller and exits 0.
func TestCLIAddKeyDispatches(t *testing.T) {
	reg := &fakeRegistry{}
	code, _, stderr := runCLI(t, Deps{Registry: reg}, "add-key", "--role", "reviewer", "--fingerprint", goodFP, "--comment", "yk")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if len(reg.added) != 1 || reg.added[0] != "reviewer "+goodFP+" yk false" {
		t.Fatalf("controller calls: %v", reg.added)
	}
}

// Verifies CLI commands (add-key): missing required flags is a usage error
// (exit 2) that never reaches the controller.
func TestCLIAddKeyMissingFlags(t *testing.T) {
	reg := &fakeRegistry{}
	code, _, _ := runCLI(t, Deps{Registry: reg}, "add-key", "--role", "reviewer")
	if code != 2 {
		t.Fatalf("missing --fingerprint must be usage error, got %d", code)
	}
	if len(reg.added) != 0 {
		t.Fatal("usage error must not reach the controller")
	}
}

// Verifies CLI commands (add-key): a RegistryUsageError from the controller
// (bad fingerprint/role) maps to exit 2; a plain error maps to exit 1.
func TestCLIAddKeyErrorMapping(t *testing.T) {
	usage := &fakeRegistry{addErr: &RegistryUsageError{Err: errors.New("bad fingerprint")}}
	if code, _, _ := runCLI(t, Deps{Registry: usage}, "add-key", "--role", "r", "--fingerprint", goodFP); code != 2 {
		t.Fatalf("RegistryUsageError must map to exit 2, got %d", code)
	}
	op := &fakeRegistry{addErr: errors.New("role already points elsewhere")}
	if code, _, _ := runCLI(t, Deps{Registry: op}, "add-key", "--role", "r", "--fingerprint", goodFP); code != 1 {
		t.Fatalf("operational error must map to exit 1, got %d", code)
	}
}

// Verifies CLI commands (list-keys): dispatches to the controller and streams
// its output to stdout.
func TestCLIListKeysDispatches(t *testing.T) {
	reg := &fakeRegistry{listOut: "reviewer  " + goodFP + "\n"}
	code, stdout, stderr := runCLI(t, Deps{Registry: reg}, "list-keys")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if stdout != "reviewer  "+goodFP+"\n" {
		t.Fatalf("stdout %q", stdout)
	}
}

// Verifies CLI commands: with no Registry wired, both subcommands report the
// unwired-seam error and exit 1 (never panic).
func TestCLIRegistryUnwired(t *testing.T) {
	if code, _, _ := runCLI(t, Deps{}, "add-key", "--role", "r", "--fingerprint", goodFP); code != 1 {
		t.Fatalf("add-key unwired: want exit 1, got %d", code)
	}
	if code, _, _ := runCLI(t, Deps{}, "list-keys"); code != 1 {
		t.Fatalf("list-keys unwired: want exit 1, got %d", code)
	}
}
