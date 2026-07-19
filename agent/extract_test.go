package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
)

var reviewSchema = OutputSchema{
	"verdict": config.FieldDef{Type: "string", Required: true, Enum: []string{"ok", "changes"}},
}

// Verifies ff8e85704b0a: the host boundary re-parses the record and returns
// it unchanged when the payload re-validates.
func TestExtractResultOK(t *testing.T) {
	dir := t.TempDir()
	rec := Result{Status: StatusOK, Payload: map[string]any{"verdict": "changes"}, Attempt: 1}
	if err := contract.WriteResultFile(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractResult(dir, reviewSchema)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK || got.Payload["verdict"] != "changes" || got.Attempt != 1 {
		t.Fatalf("extract = %+v", got)
	}
}

// Verifies ff8e85704b0a: a missing or truncated record is synthesized as a
// box-vanished failure — no path yields zero records.
func TestExtractResultBoxVanished(t *testing.T) {
	t.Run("missing record", func(t *testing.T) {
		got, err := ExtractResult(t.TempDir(), reviewSchema)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusFailed || got.Error.Reason != contract.ReasonBoxVanished {
			t.Fatalf("extract = %+v", got)
		}
	})
	t.Run("truncated record", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, contract.ResultFile), []byte(`{"status": "ok", "payl`), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := ExtractResult(dir, reviewSchema)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusFailed || got.Error.Reason != contract.ReasonBoxVanished {
			t.Fatalf("extract = %+v", got)
		}
	})
}

// Verifies ff8e85704b0a: the box is untrusted — a record whose payload was
// tampered to break the schema becomes a failed record the host never
// threads.
func TestExtractResultRevalidatesTamperedPayload(t *testing.T) {
	dir := t.TempDir()
	rec := Result{Status: StatusOK, Payload: map[string]any{"verdict": "forged-value"}, Attempt: 1}
	if err := contract.WriteResultFile(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractResult(dir, reviewSchema)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.Error.Reason != contract.ReasonOutputSchema {
		t.Fatalf("extract = %+v, want host-side output-schema failure", got)
	}
	if got.Payload != nil {
		t.Fatal("a tampered payload must not survive extraction")
	}
	if !strings.Contains(got.Error.Detail, "host re-validation") {
		t.Fatalf("detail = %q", got.Error.Detail)
	}
}

// Verifies ff8e85704b0a: a failed record passes through, but its payload is
// never threadable.
func TestExtractResultFailedPassThrough(t *testing.T) {
	dir := t.TempDir()
	rec := Result{
		Status:  StatusFailed,
		Payload: map[string]any{"sneak": true},
		Error:   &ResultError{Reason: contract.ReasonAgentFailed, Handoff: contract.HandoffFile},
		Attempt: 2,
	}
	if err := contract.WriteResultFile(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractResult(dir, reviewSchema)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.Error.Reason != contract.ReasonAgentFailed || got.Payload != nil {
		t.Fatalf("extract = %+v", got)
	}
}

// Verifies f1ce19e94daa (first pass): the host recomputes the unthreaded set
// rather than trusting the box's — extras stay in the record, invisible to
// wiring.
func TestExtractResultRecomputesUnthreaded(t *testing.T) {
	dir := t.TempDir()
	rec := Result{Status: StatusOK, Payload: map[string]any{"verdict": "ok", "surplus": "x"}, Attempt: 1}
	if err := contract.WriteResultFile(dir, rec); err != nil {
		t.Fatal(err)
	}
	got, err := ExtractResult(dir, reviewSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Unthreaded) != 1 || got.Unthreaded[0] != "surplus" {
		t.Fatalf("unthreaded = %v", got.Unthreaded)
	}
}

// Verifies ff8e85704b0a (§1 contract handshake): the host asserts the
// record's stamped contract version on extract — an unstamped record (a
// writer that predates stamping) and a wrong stamp both surface as a
// contract-version failure naming FABER_BOX_BIN, never interpreted as if
// they spoke this contract.
func TestExtractAssertsContractVersion(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"status":"ok","payload":{"field":"v"},"attempt":1}`) // no contract stamp
	if err := os.WriteFile(filepath.Join(dir, contract.ResultFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := ExtractResult(dir, OutputSchema{"field": config.FieldDef{Type: "string"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || res.Error.Reason != contract.ReasonContractVersion {
		t.Fatalf("unstamped record must fail contract-version, got %+v", res)
	}
	if !strings.Contains(res.Error.Detail, "FABER_BOX_BIN") {
		t.Fatalf("detail must point at FABER_BOX_BIN: %s", res.Error.Detail)
	}

	// A wrong stamp likewise.
	raw = []byte(`{"status":"ok","contract":99,"payload":{"field":"v"},"attempt":1}`)
	if err := os.WriteFile(filepath.Join(dir, contract.ResultFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = ExtractResult(dir, OutputSchema{"field": config.FieldDef{Type: "string"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || res.Error.Reason != contract.ReasonContractVersion {
		t.Fatalf("mismatched stamp must fail contract-version, got %+v", res)
	}

	// The stamped writer (WriteResultFile) passes.
	if err := contract.WriteResultFile(dir, contract.Result{
		Status: contract.StatusOK, Payload: map[string]any{"field": "v"}, Attempt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	res, err = ExtractResult(dir, OutputSchema{"field": config.FieldDef{Type: "string"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK {
		t.Fatalf("stamped record must extract ok, got %+v", res)
	}
}
