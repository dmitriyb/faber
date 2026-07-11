package failure

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func okResult(payload string) Result {
	return Result{
		Status:  StatusOK,
		Payload: json.RawMessage(payload),
		Timing:  fixedTiming(1),
		Attempt: 1,
	}
}

func failedResult(reason, detail string) Result {
	return Result{
		Status:  StatusFailed,
		Error:   &ErrorRecord{Reason: reason, Detail: detail},
		Timing:  fixedTiming(1),
		Attempt: 1,
	}
}

// fixedTiming returns a deterministic timing so records can be compared
// byte-for-byte across policy runs.
func fixedTiming(attempt int) Timing {
	base := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	return Timing{
		Started:  base.Add(time.Duration(attempt) * time.Minute),
		Finished: base.Add(time.Duration(attempt)*time.Minute + 30*time.Second),
	}
}

// Verifies 3b7d2586b5ae: the result record is a strict union — ok carries a
// payload and no error, failed carries an error (with a reason) and no
// payload, attempts are 1-based, and the status vocabulary is closed.
func TestResultValidateUnion(t *testing.T) {
	valid := func(mut func(*Result)) Result {
		r := okResult(`{"field":"value"}`)
		mut(&r)
		return r
	}
	tests := []struct {
		name    string
		res     Result
		wantErr string
	}{
		{"ok with payload", okResult(`{"field":1}`), ""},
		{"failed with error", failedResult(ReasonAgent, "the agent phase died"), ""},
		{"ok without payload", valid(func(r *Result) { r.Payload = nil }), "requires a payload"},
		{"ok with error", valid(func(r *Result) { r.Error = &ErrorRecord{Reason: ReasonHook, Detail: "x"} }), "must not carry an error"},
		{"failed without error", Result{Status: StatusFailed, Timing: fixedTiming(1), Attempt: 1}, "requires an error record"},
		{"failed with payload", valid(func(r *Result) {
			r.Status = StatusFailed
			r.Error = &ErrorRecord{Reason: ReasonAgent, Detail: "x"}
		}), "must not carry a payload"},
		{"failed with empty reason", failedResult("", "detail"), "requires a reason"},
		{"attempt zero", valid(func(r *Result) { r.Attempt = 0 }), "not 1-based"},
		{"unknown status", valid(func(r *Result) { r.Status = "maybe" }), `unknown status "maybe"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.res.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// Verifies 3b7d2586b5ae: an unfavorable-but-valid payload is status ok —
// failure semantics never inspect the payload, so the record validates and
// the policy consumes no retry and runs no cleanup.
func TestUnfavorableIsNotFailure(t *testing.T) {
	res := okResult(`{"outcome":"unfavorable"}`)
	if err := res.Validate(); err != nil {
		t.Fatalf("unfavorable payload must validate as ok: %v", err)
	}

	hooks := &recordingHooks{}
	p := &Policy{Hooks: hooks}
	runner, calls := scriptedRunner(t, res)
	got := p.RunStep(t.Context(), StepSpec{ID: "task/one", Retry: 2, OnFailure: "hook.sh"}, runner)
	if got.Status != StatusOK || got.Attempt != 1 || len(got.Attempts) != 0 {
		t.Fatalf("want single ok attempt, got %+v", got)
	}
	if *calls != 1 {
		t.Fatalf("want exactly one attempt, runner ran %d times", *calls)
	}
	if len(hooks.invocations) != 0 {
		t.Fatalf("cleanup must not run for an ok result, ran %d times", len(hooks.invocations))
	}
}

// Verifies 3b7d2586b5ae: the engine's minimal fallback record for an agent
// that succeeded but wrote no result file — status ok with an empty-object
// payload — validates and journals like any other record.
func TestFallbackRecordValidates(t *testing.T) {
	res := okResult(`{}`)
	if err := res.Validate(); err != nil {
		t.Fatalf("fallback record must validate: %v", err)
	}
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(Header{RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if err := j.AppendResult(ResultRecord{StepID: "task/one", InputHash: "h", Result: res}); err != nil {
		t.Fatalf("fallback record must journal: %v", err)
	}
	rp, err := store.Load("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rp.Results[Key{"task/one", "h"}]; !ok {
		t.Fatal("fallback record missing after replay")
	}
}

// Verifies 3b7d2586b5ae: the JSON encoding keeps the union clean — an ok line
// has no error field, a failed line has no payload field, and a record
// round-trips unchanged.
func TestResultJSONRoundTrip(t *testing.T) {
	okLine, err := json.Marshal(okResult(`{"field":"v"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(okLine), `"error"`) {
		t.Fatalf("ok record must omit error: %s", okLine)
	}
	failedLine, err := json.Marshal(failedResult(ReasonAgent, "died"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(failedLine), `"payload"`) {
		t.Fatalf("failed record must omit payload: %s", failedLine)
	}
	var back Result
	if err := json.Unmarshal(failedLine, &back); err != nil {
		t.Fatal(err)
	}
	if back.Error == nil || back.Error.Reason != ReasonAgent {
		t.Fatalf("round trip lost the error record: %+v", back)
	}
}
