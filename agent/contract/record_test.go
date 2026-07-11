package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Verifies ff8e85704b0a: the attempt record round-trips through the result
// file, and the write is atomic within the result directory (temp file plus
// rename — no half-written record ever carries the well-known name).
func TestResultFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec := Result{
		Status:     StatusOK,
		Payload:    map[string]any{"verdict": "ok"},
		Unthreaded: []string{"surplus"},
		Timing:     map[string]time.Duration{"agent": 2 * time.Second},
		Attempt:    2,
	}
	if err := WriteResultFile(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResultFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK || got.Payload["verdict"] != "ok" || got.Attempt != 2 || got.Timing["agent"] != 2*time.Second {
		t.Fatalf("round trip = %+v", got)
	}
	// No stray temp files remain beside the record.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != ResultFile {
		t.Fatalf("result dir entries = %v, want exactly %s", entries, ResultFile)
	}
}

// Verifies ff8e85704b0a: a record with an unknown status is unreadable —
// the host boundary treats it as no record at all.
func TestReadResultFileRejectsUnknownStatus(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ResultFile), []byte(`{"status": "odd"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadResultFile(dir); err == nil || !strings.Contains(err.Error(), "unknown status") {
		t.Fatalf("err = %v", err)
	}
}

// Verifies b880aa49b3b9: the handoff record's shape is stable — phase,
// reason, exit code, stderr tail, secret-free inputs, workdir.
func TestHandoffFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := Handoff{
		Phase:      "prelude",
		Reason:     ReasonHookFailed,
		ExitCode:   3,
		StderrTail: "broke\n",
		Inputs:     map[string]string{"ALPHA": "v1"},
		Workdir:    "/workspace/repo-a",
	}
	if err := WriteHandoffFile(dir, h); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, HandoffFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"phase": "prelude"`, `"reason": "hook-failed"`, `"exit_code": 3`, `"ALPHA": "v1"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("handoff JSON misses %q:\n%s", want, raw)
		}
	}
}

// Verifies 93ba0858d75f: slot names map onto the env contract with the one
// defined substitution.
func TestInputEnvNaming(t *testing.T) {
	if got := InputEnv("work-item"); got != "FABER_INPUT_WORK_ITEM" {
		t.Fatalf("InputEnv = %q", got)
	}
	if got := SlotToken("repo"); got != "REPO" {
		t.Fatalf("SlotToken = %q", got)
	}
}
