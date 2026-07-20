package failure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
)

// scriptedRunner is the fixture StepRunner: per-attempt outcomes declared up
// front, each producing a well-formed Result with deterministic timing. It
// returns the runner and a pointer to its call count.
func scriptedRunner(t *testing.T, outcomes ...Result) (StepRunner, *int) {
	t.Helper()
	calls := new(int)
	return func(ctx context.Context, attempt int) (Result, error) {
		*calls++
		if *calls != attempt {
			t.Fatalf("attempt accounting drifted: call %d ran as attempt %d", *calls, attempt)
		}
		if *calls > len(outcomes) {
			t.Fatalf("runner called %d times, only %d outcomes scripted", *calls, len(outcomes))
		}
		return outcomes[*calls-1], nil
	}, calls
}

// recordingHooks is the fixture HookRunner: it records every invocation and
// returns a configurable error.
type recordingHooks struct {
	invocations []HookInvocation
	err         error
}

func (r *recordingHooks) RunOnFailure(_ context.Context, inv HookInvocation) error {
	r.invocations = append(r.invocations, inv)
	return r.err
}

// testJournal opens a journal in a temp run dir and returns it with its store
// and run id.
func testJournal(t *testing.T) (*Store, *Journal, string) {
	t.Helper()
	store := NewStore(t.TempDir(), nil)
	runID := "run-1"
	j, err := store.Begin(Header{RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return store, j, runID
}

// Verifies 9cb70f998931: retry: 2 over fail, fail, ok yields three fresh
// attempts, cleanup exactly twice (between 1→2 and 2→3), and a single final
// record ok, attempt 3, with a two-entry attempt history.
func TestRetryEventualSuccess(t *testing.T) {
	store, j, runID := testJournal(t)
	hooks := &recordingHooks{}
	p := &Policy{Journal: j, Hooks: hooks}
	runner, calls := scriptedRunner(t,
		failedResult(ReasonAgent, "attempt 1 died"),
		failedResult(ReasonHook, "attempt 2 hook refused"),
		okResult(`{"field":"settled"}`),
	)
	spec := StepSpec{ID: "task/one", InputHash: "h1", Retry: 2, OnFailure: "hook.sh"}

	res := p.RunStep(t.Context(), spec, runner)

	if *calls != 3 {
		t.Fatalf("want 3 attempts, got %d", *calls)
	}
	if res.Status != StatusOK || res.Attempt != 3 {
		t.Fatalf("want ok attempt 3, got %+v", res)
	}
	if len(res.Attempts) != 2 ||
		res.Attempts[0].Error.Reason != ReasonAgent ||
		res.Attempts[1].Error.Reason != ReasonHook {
		t.Fatalf("want two-entry attempt history (agent, hook), got %+v", res.Attempts)
	}
	if len(hooks.invocations) != 2 {
		t.Fatalf("want cleanup between attempts only (2 runs), got %d", len(hooks.invocations))
	}
	if hooks.invocations[0].Attempt != 1 || hooks.invocations[1].Attempt != 2 {
		t.Fatalf("cleanup attempts want 1,2 got %+v", hooks.invocations)
	}
	// Journal: the caller appends the single final result; the policy already
	// appended one cleanup record per hook run.
	if err := j.AppendResult(ResultRecord{StepID: spec.ID, InputHash: spec.InputHash, Result: res}); err != nil {
		t.Fatal(err)
	}
	rp, err := store.Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Results) != 1 || len(rp.Cleanups) != 2 {
		t.Fatalf("want 1 result + 2 cleanup records, got %d + %d", len(rp.Results), len(rp.Cleanups))
	}
	got := rp.Results[Key{spec.ID, spec.InputHash}]
	if got.Result.Status != StatusOK || got.Result.Attempt != 3 || len(got.Result.Attempts) != 2 {
		t.Fatalf("journaled record lost attempt history: %+v", got.Result)
	}
}

// Verifies 9cb70f998931: exhausted retries — fail, fail, fail under retry: 2
// — run cleanup three times (twice between attempts, once terminal) and
// settle a single final failure record with attempt 3 and two-entry history.
func TestRetryExhausted(t *testing.T) {
	_, j, _ := testJournal(t)
	hooks := &recordingHooks{}
	p := &Policy{Journal: j, Hooks: hooks}
	runner, calls := scriptedRunner(t,
		failedResult(ReasonAgent, "1"),
		failedResult(ReasonAgent, "2"),
		failedResult(ReasonAgent, "3"),
	)
	res := p.RunStep(t.Context(), StepSpec{ID: "task/one", InputHash: "h1", Retry: 2, OnFailure: "hook.sh"}, runner)

	if *calls != 3 {
		t.Fatalf("want 3 attempts, got %d", *calls)
	}
	if res.Status != StatusFailed || res.Attempt != 3 || len(res.Attempts) != 2 {
		t.Fatalf("want final failure attempt 3 with 2-entry history, got %+v", res)
	}
	if len(hooks.invocations) != 3 {
		t.Fatalf("want 3 cleanup runs (2 between + 1 terminal), got %d", len(hooks.invocations))
	}
	if hooks.invocations[2].Attempt != 3 {
		t.Fatalf("terminal cleanup should follow attempt 3, got %+v", hooks.invocations[2])
	}
}

// Verifies 9cb70f998931: retry: 0 (or absent) means exactly one attempt and
// an empty attempt history; a lone failure still gets its terminal cleanup.
func TestRetryZeroSingleAttempt(t *testing.T) {
	hooks := &recordingHooks{}
	p := &Policy{Hooks: hooks}
	runner, calls := scriptedRunner(t, failedResult(ReasonAgent, "died"))
	res := p.RunStep(t.Context(), StepSpec{ID: "task/one", OnFailure: "hook.sh"}, runner)
	if *calls != 1 || res.Attempt != 1 || len(res.Attempts) != 0 {
		t.Fatalf("want one attempt with empty history, got calls=%d res=%+v", *calls, res)
	}
	if len(hooks.invocations) != 1 {
		t.Fatalf("want exactly the terminal cleanup, got %d", len(hooks.invocations))
	}
}

// Verifies 9cb70f998931: retry without on_failure is accepted — attempts
// proceed with no cleanup records; the idempotency burden is the user's.
func TestRetryWithoutHook(t *testing.T) {
	store, j, runID := testJournal(t)
	p := &Policy{Journal: j, Hooks: &recordingHooks{}}
	runner, calls := scriptedRunner(t,
		failedResult(ReasonAgent, "1"),
		okResult(`{"field":1}`),
	)
	res := p.RunStep(t.Context(), StepSpec{ID: "task/one", InputHash: "h1", Retry: 1}, runner)
	if *calls != 2 || res.Status != StatusOK {
		t.Fatalf("want retry to proceed without hook, got calls=%d res=%+v", *calls, res)
	}
	rp, err := store.Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Cleanups) != 0 {
		t.Fatalf("want no cleanup records without on_failure, got %d", len(rp.Cleanups))
	}
}

// Verifies 9796e2bddf7a: cleanup failure is reported but never masks — with
// the hook failing, a cleanup record (ok=false) is journaled and the step's
// final failure record is byte-identical to the same run without a hook.
func TestCleanupFailureNeverMasks(t *testing.T) {
	run := func(hooks HookRunner, onFailure string) (Result, *Replay) {
		t.Helper()
		store, j, runID := testJournal(t)
		p := &Policy{Journal: j, Hooks: hooks}
		runner, _ := scriptedRunner(t, failedResult(ReasonAgent, "the original failure"))
		res := p.RunStep(t.Context(), StepSpec{ID: "task/one", InputHash: "h1", Inputs: map[string]any{"slot": "v"}, OnFailure: onFailure}, runner)
		rp, err := store.Load(runID)
		if err != nil {
			t.Fatal(err)
		}
		return res, rp
	}

	withHook, rpHook := run(&recordingHooks{err: errors.New("exit status 1")}, "hook.sh")
	withoutHook, rpNone := run(nil, "")

	a, err := json.Marshal(withHook)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(withoutHook)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("failed hook must not touch the original failure record:\nwith:    %s\nwithout: %s", a, b)
	}
	if len(rpHook.Cleanups) != 1 || rpHook.Cleanups[0].OK || !strings.Contains(rpHook.Cleanups[0].Detail, "exit status 1") {
		t.Fatalf("want journaled cleanup record ok=false, got %+v", rpHook.Cleanups)
	}
	if len(rpNone.Cleanups) != 0 {
		t.Fatalf("no-hook run journaled cleanup records: %+v", rpNone.Cleanups)
	}
}

// Verifies 9796e2bddf7a: the exec hook contract — the opaque script runs
// host-side and receives every resolved input as FABER_INPUT_* env (scalars
// verbatim, objects as JSON), the step id and attempt, and the exact error
// record as JSON on stdin.
func TestExecHookContract(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cleanup.sh")
	writeScript(t, script, `#!/bin/sh
env > "$FABER_TEST_OUT/hook.env"
cat > "$FABER_TEST_OUT/hook.stdin"
exit 0
`)
	t.Setenv("FABER_TEST_OUT", dir)

	errRec := ErrorRecord{Reason: ReasonAgent, Detail: "died", Handoff: "handoff/task-one"}
	inv := HookInvocation{
		Script:  script,
		StepID:  "task/one",
		Attempt: 2,
		Inputs: map[string]any{
			"text":   "plain value",
			"count":  7,
			"nested": map[string]any{"key": "value"},
		},
		Error: errRec,
	}
	r := &ExecHookRunner{}
	if err := r.RunOnFailure(t.Context(), inv); err != nil {
		t.Fatalf("hook run: %v", err)
	}

	envBytes, err := os.ReadFile(filepath.Join(dir, "hook.env"))
	if err != nil {
		t.Fatal(err)
	}
	envs := string(envBytes)
	for _, want := range []string{
		"FABER_STEP_ID=task/one",
		"FABER_ATTEMPT=2",
		"FABER_INPUT_TEXT=plain value",
		"FABER_INPUT_COUNT=7",
		`FABER_INPUT_NESTED={"key":"value"}`,
	} {
		if !strings.Contains(envs, want+"\n") {
			t.Errorf("hook env missing %q\nenv:\n%s", want, envs)
		}
	}

	stdin, err := os.ReadFile(filepath.Join(dir, "hook.stdin"))
	if err != nil {
		t.Fatal(err)
	}
	var got ErrorRecord
	if err := json.Unmarshal(stdin, &got); err != nil {
		t.Fatalf("hook stdin is not the error record JSON: %v (%s)", err, stdin)
	}
	if !reflect.DeepEqual(got, errRec) {
		t.Fatalf("hook stdin error record: want %+v got %+v", errRec, got)
	}
}

// Verifies 9796e2bddf7a: a hook exiting non-zero surfaces as a cleanup
// failure carrying the exit status and captured output.
func TestExecHookNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cleanup.sh")
	writeScript(t, script, "#!/bin/sh\necho cannot release item >&2\nexit 3\n")
	r := &ExecHookRunner{}
	err := r.RunOnFailure(t.Context(), HookInvocation{Script: script, StepID: "task/one", Attempt: 1, Error: ErrorRecord{Reason: ReasonAgent, Detail: "x"}})
	if err == nil {
		t.Fatal("want error from exit 3")
	}
	if !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "cannot release item") {
		t.Fatalf("want exit status and output in error, got: %v", err)
	}
}

// Verifies 3b7d2586b5ae: a launch-level error (the attempt never started) is
// still a record, not an absence — status failed with reason "launch".
func TestLaunchErrorBecomesRecord(t *testing.T) {
	p := &Policy{}
	res := p.RunStep(t.Context(), StepSpec{ID: "task/one"}, func(ctx context.Context, attempt int) (Result, error) {
		return Result{}, errors.New("container never launched")
	})
	if res.Status != StatusFailed || res.Error == nil || res.Error.Reason != ReasonLaunch {
		t.Fatalf("want failed/launch record, got %+v", res)
	}
	if !strings.Contains(res.Error.Detail, "container never launched") {
		t.Fatalf("launch detail lost: %+v", res.Error)
	}
	if res.Timing.Started.IsZero() || res.Timing.Finished.IsZero() {
		t.Fatalf("launch record must carry timing: %+v", res.Timing)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("launch record must validate: %v", err)
	}
}

// Verifies 3b7d2586b5ae: a runner that breaks the result union is coerced
// into a failed record (the step did not produce what its contract promises)
// rather than propagating an invalid record to any consumer.
func TestInvalidRunnerRecordCoerced(t *testing.T) {
	p := &Policy{}
	res := p.RunStep(t.Context(), StepSpec{ID: "task/one"}, func(ctx context.Context, attempt int) (Result, error) {
		return Result{Status: StatusOK, Timing: fixedTiming(1)}, nil // ok without payload
	})
	if res.Status != StatusFailed || res.Error == nil || res.Error.Reason != ReasonResultSchema {
		t.Fatalf("want failed/result-schema record, got %+v", res)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("coerced record must validate: %v", err)
	}
}

// Verifies 9796e2bddf7a: fail-stop with independent branches — in the diamond
// a→b, a→c, b→d, c→d with b failed, d is in the skip set and c is not; a step
// whose `when` reads the failed step's result is skipped too; the failed node
// itself is failed, not skipped.
func TestFailStopSkipSet(t *testing.T) {
	ir := &config.IR{
		IRVersion: 1,
		Workflow:  "w",
		Nodes: []config.Node{
			{ID: "task/a", Kind: config.KindAgent},
			{ID: "task/b", Kind: config.KindAgent},
			{ID: "task/c", Kind: config.KindAgent},
			{ID: "task/d", Kind: config.KindAgent},
			{ID: "task/e", Kind: config.KindAgent, When: &config.CondSpec{CEL: `steps["task/b"].field == 1`, Deps: []string{"task/b"}}},
		},
		Edges: []config.Edge{
			{From: "task/a", FromPort: "out", To: "task/b", ToPort: "in"},
			{From: "task/a", FromPort: "out", To: "task/c", ToPort: "in"},
			{From: "task/b", FromPort: "out", To: "task/d", ToPort: "in"},
			{From: "task/c", FromPort: "out", To: "task/d", ToPort: "in"},
		},
	}
	skip := SkipSet(ir, "task/b")
	want := map[string]bool{"task/d": true, "task/e": true}
	if !reflect.DeepEqual(skip, want) {
		t.Fatalf("skip set: want %v got %v", want, skip)
	}
	if skip["task/c"] {
		t.Fatal("independent branch c must keep running")
	}
	if skip["task/b"] {
		t.Fatal("the failed node is failed, not skipped")
	}
	if StateSkipped != "skipped (dependency failed)" {
		t.Fatalf("terminal vocabulary drifted: %q", StateSkipped)
	}
}

// Verifies bff0f92afc29: journal records and policy logs never carry resolved
// input values — only step ids, hashes, attempts, and error text — so a
// secret-bearing input never lands in durable state by this module's hand.
func TestNoInputValuesInJournalOrLogs(t *testing.T) {
	const sentinel = "SENTINEL-INPUT-VALUE"
	store, j, runID := testJournal(t)
	var logBuf bytes.Buffer
	p := &Policy{
		Journal: j,
		Hooks:   &recordingHooks{err: errors.New("cleanup failed")},
		Log:     slog.New(slog.NewJSONHandler(&logBuf, nil)),
	}
	runner, _ := scriptedRunner(t, failedResult(ReasonAgent, "died"))
	spec := StepSpec{ID: "task/one", InputHash: "h1", Inputs: map[string]any{"token_slot": sentinel}, OnFailure: "hook.sh"}
	res := p.RunStep(t.Context(), spec, runner)
	if err := j.AppendResult(ResultRecord{StepID: spec.ID, InputHash: spec.InputHash, Result: res}); err != nil {
		t.Fatal(err)
	}
	journalBytes, err := os.ReadFile(filepath.Join(store.RunDir(runID), "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journalBytes, []byte(sentinel)) {
		t.Fatal("journal contains a resolved input value")
	}
	if bytes.Contains(logBuf.Bytes(), []byte(sentinel)) {
		t.Fatal("policy log contains a resolved input value")
	}
}

// Verifies 690a06d8a44b (first pass): there is no step timeout — abort is
// process-level, expressed as context propagation into the hook subprocess:
// a cancelled context kills a hung hook promptly.
func TestProcessLevelAbortKillsHook(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hang.sh")
	writeScript(t, script, "#!/bin/sh\nsleep 30\n")
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	r := &ExecHookRunner{}
	start := time.Now()
	err := r.RunOnFailure(ctx, HookInvocation{Script: script, StepID: "task/one", Attempt: 1, Error: ErrorRecord{Reason: ReasonAgent, Detail: "x"}})
	if err == nil {
		t.Fatal("want error from cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("hook was not killed promptly: %v", elapsed)
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Verifies 9796e2bddf7a: two resolved inputs whose names collapse to the same
// FABER_INPUT_* env var are refused with an error naming both slots — os/exec
// would keep the last duplicate and silently shadow an input the cleanup
// script acts on.
func TestExecHookInputNameCollision(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "cleanup.sh")
	writeScript(t, script, "#!/bin/sh\nexit 0\n")
	r := &ExecHookRunner{}
	err := r.RunOnFailure(t.Context(), HookInvocation{
		Script:  script,
		StepID:  "task/one",
		Attempt: 1,
		Inputs:  map[string]any{"slot-a": "first", "slot_a": "second"},
		Error:   ErrorRecord{Reason: ReasonAgent, Detail: "x"},
	})
	if err == nil {
		t.Fatal("want collision error")
	}
	for _, want := range []string{`"slot-a"`, `"slot_a"`, "FABER_INPUT_SLOT_A"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("collision error must name %s, got: %v", want, err)
		}
	}
}

// Verifies 9cb70f998931 (CC-F4): the retry loop short-circuits on a cancelled
// context — remaining attempts are never launched against a dead context
// (each would fully stage skills, bindings, and a docker kill), and the
// canceled record carries the real attempts' history.
func TestRetryCancelShortCircuit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	runner := func(_ context.Context, attempt int) (Result, error) {
		calls++
		cancel() // the run is aborted while the first attempt is in flight
		return failedResult(ReasonAgent, "killed mid-attempt"), nil
	}
	p := &Policy{}
	res := p.RunStep(ctx, StepSpec{ID: "task/x", Retry: 5}, runner)
	if calls != 1 {
		t.Fatalf("runner ran %d times, want 1 — no attempt may launch after cancellation", calls)
	}
	if res.Status != StatusFailed || res.Error.Reason != ReasonCanceled {
		t.Fatalf("want a canceled record, got %+v", res)
	}
	if len(res.Attempts) != 1 || res.Attempts[0].Error.Reason != ReasonAgent {
		t.Fatalf("the real attempt's history must ride along, got %+v", res.Attempts)
	}

	// Cancelled before the first attempt: nothing runs at all.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	calls2 := 0
	res = p.RunStep(ctx2, StepSpec{ID: "task/y", Retry: 2}, func(context.Context, int) (Result, error) {
		calls2++
		return okResult(`{}`), nil
	})
	if calls2 != 0 || res.Error == nil || res.Error.Reason != ReasonCanceled {
		t.Fatalf("pre-cancelled step must settle canceled without running (calls=%d, res=%+v)", calls2, res)
	}
}

// ctxCheckingHooks reports the liveness of the context each invocation ran on.
type ctxCheckingHooks struct{ errs []error }

func (h *ctxCheckingHooks) RunOnFailure(ctx context.Context, _ HookInvocation) error {
	h.errs = append(h.errs, ctx.Err())
	return ctx.Err()
}

// Verifies 9796e2bddf7a (L-P3e): on_failure cleanup runs on a
// cancellation-detached, time-bounded context — an aborted run is exactly
// when releasing external side-effects matters, so cleanup must not inherit
// the dead run context.
func TestCleanupRunsOnDetachedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hooks := &ctxCheckingHooks{}
	p := &Policy{Hooks: hooks}
	calls := 0
	runner := func(context.Context, int) (Result, error) {
		calls++
		cancel() // the operator aborts while the only attempt is in flight
		return failedResult(ReasonAgent, "killed"), nil
	}
	res := p.RunStep(ctx, StepSpec{ID: "task/x", OnFailure: "./cleanup"}, runner)
	if calls != 1 || res.Status != StatusFailed {
		t.Fatalf("unexpected run shape: calls=%d res=%+v", calls, res)
	}
	if len(hooks.errs) != 1 {
		t.Fatalf("terminal cleanup must run exactly once, ran %d times", len(hooks.errs))
	}
	if hooks.errs[0] != nil {
		t.Fatalf("cleanup saw a dead context: %v", hooks.errs[0])
	}
}
