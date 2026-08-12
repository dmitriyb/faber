package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
)

// writeHalt writes a halt file into the result dir, as a hook or the agent
// skill would.
func writeHalt(t *testing.T, d testDirs, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.result, contract.HaltFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Verifies: a prelude that writes halt.json and exits 0 settles the step
// halted — the agent and the postlude never run, the record carries the
// halter's reason with the requesting phase stamped, and the box exits 0
// (the record, not the exit code, is authoritative). The halting prelude
// owes no bundle: no CONTEXT.md is written and the step still halts, not
// bundle-missing.
func TestHaltFromPreludeSkipsAgentAndPostlude(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	writeHook(t, d, contract.HookPostlude)
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == prelude {
			writeHalt(t, d, `{"reason":"needs-triage","detail":"ci stuck past the retry budget"}`)
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, nil, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0 (halt is not a failure)", code)
	}
	for _, call := range fr.argvs() {
		if strings.HasPrefix(call, "agent-cli") || strings.Contains(call, "postlude") {
			t.Fatalf("agent/postlude ran after a prelude halt: %v", fr.argvs())
		}
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusHalted {
		t.Fatalf("record status = %q, want halted", rec.Status)
	}
	if rec.Halt == nil || rec.Halt.Reason != "needs-triage" || rec.Halt.Phase != contract.HookPrelude {
		t.Fatalf("halt arm = %+v, want reason needs-triage from phase prelude", rec.Halt)
	}
	if rec.Halt.Detail != "ci stuck past the retry budget" {
		t.Fatalf("halt detail lost: %+v", rec.Halt)
	}
	if rec.Error != nil {
		t.Fatalf("halted record must carry no error: %+v", rec.Error)
	}
}

// Verifies: the agent's skill can halt too — the file appearing during the
// agent phase settles the step halted after that phase, the postlude never
// runs, and the phase is stamped as agent.
func TestHaltFromAgentSkipsPostlude(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	writeHook(t, d, contract.HookPostlude)
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeHalt(t, d, `{"reason":"findings-posted"}`)
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, nil, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, call := range fr.argvs() {
		if strings.Contains(call, "postlude") {
			t.Fatalf("postlude ran after an agent halt: %v", fr.argvs())
		}
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusHalted || rec.Halt == nil || rec.Halt.Phase != "agent" {
		t.Fatalf("record = %+v, want halted from phase agent", rec)
	}
}

// Verifies: a malformed halt file — unparseable bytes or a missing reason —
// fails the step loudly with reason halt-invalid instead of guessing an
// operator-stop; the failing phase is the one after which the file was
// found.
func TestMalformedHaltFileFailsLoudly(t *testing.T) {
	tests := []struct {
		name, content, wantDetail string
	}{
		{"not json", "not-json", "not a JSON object"},
		{"no reason", `{"detail":"why though"}`, "names no reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDirs(t)
			fr := &fakeRunner{}
			prelude := writeHook(t, d, contract.HookPrelude)
			fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
				if spec.Argv[0] == prelude {
					writeHalt(t, d, tt.content)
				}
				return CmdResult{}, nil
			}
			b := newTestBox(t, d, nil, fr)
			if code := Main(context.Background(), b); code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			rec := readRecord(t, d)
			if rec.Status != contract.StatusFailed || rec.Error == nil {
				t.Fatalf("record = %+v, want failed", rec)
			}
			if rec.Error.Reason != contract.ReasonHaltInvalid {
				t.Fatalf("reason = %q, want %q", rec.Error.Reason, contract.ReasonHaltInvalid)
			}
			if !strings.Contains(rec.Error.Detail, tt.wantDetail) {
				t.Fatalf("detail = %q, want it to contain %q", rec.Error.Detail, tt.wantDetail)
			}
			if h := readHandoff(t, d); h.Phase != contract.HookPrelude {
				t.Fatalf("handoff phase = %q, want prelude", h.Phase)
			}
		})
	}
}

// Verifies: a phase that fails after writing halt.json is a failure, never a
// halt — the halt request is honored only from an orderly exit, so exit
// status keeps meaning pass/fail.
func TestFailingPhaseOutranksHaltFile(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == prelude {
			writeHalt(t, d, `{"reason":"needs-triage"}`)
			return CmdResult{ExitCode: 3, StderrTail: []byte("prelude broke\n")}, nil
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, nil, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusFailed || rec.Error == nil || rec.Error.Reason != contract.ReasonHookFailed {
		t.Fatalf("record = %+v, want the ordinary hook failure", rec)
	}
	if rec.Halt != nil {
		t.Fatalf("a failing phase must never settle halted: %+v", rec)
	}
}

// Verifies: without a halt file, nothing changes — the happy path settles ok
// and the halt check leaves no trace (guards against the check misfiring on
// an absent file).
func TestNoHaltFileIsANoOp(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{}`)
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, nil, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if rec := readRecord(t, d); rec.Status != contract.StatusOK {
		t.Fatalf("record status = %q, want ok", rec.Status)
	}
}
