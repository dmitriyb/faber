package box

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
)

// skipPreludeHandler returns a fake-runner handler whose prelude writes the
// bundle (CONTEXT.md + a sidecar carrying the skip request and one opaque
// value) and whose optional postlude/agent behave per the flags.
func skipPreludeHandler(t *testing.T, d testDirs, prelude string, skipValue string, agentWrites bool) func(CmdSpec, bool) (CmdResult, error) {
	t.Helper()
	return func(spec CmdSpec, stream bool) (CmdResult, error) {
		switch spec.Argv[0] {
		case prelude:
			if err := os.WriteFile(filepath.Join(d.bundle, contract.ContextDoc), []byte("body\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			sidecar := "DERIVED=v1\n" + contract.SkipAgentKey + "=" + skipValue + "\n"
			if err := os.WriteFile(filepath.Join(d.bundle, contract.BundleEnvFile), []byte(sidecar), 0o644); err != nil {
				t.Fatal(err)
			}
		case "agent-cli":
			if agentWrites {
				writeOutput(t, d, `{}`)
			}
		}
		return CmdResult{}, nil
	}
}

// Verifies ae434449cac9: on an opted-in template, the prelude's skip request
// makes the agent phase a no-op — no agent process, the postlude still runs,
// the record is ok with agent_skipped, and the skip key was consumed (never
// exported into the postlude's environment).
func TestSkipAgentOnOptedInTemplate(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	postlude := writeHook(t, d, contract.HookPostlude)
	base := skipPreludeHandler(t, d, prelude, "1", false)
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == postlude {
			// The prelude (or postlude) owes the outputs when the agent is
			// skipped; this postlude satisfies the contract.
			writeOutput(t, d, `{}`)
		}
		return base(spec, stream)
	}
	b := newTestBox(t, d, map[string]string{contract.EnvAgentOptional: "1"}, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	// Hooks receive the box environment only (sidecar values merge solely
	// into the agent env), so the meaningful skip-key leak check is the
	// agent-env assertion in TestSkipRequestIgnoredWithoutOptIn.
	var postludeRan bool
	for _, call := range fr.calls {
		if call.Argv[0] == "agent-cli" {
			t.Fatalf("agent ran despite an honored skip: %v", fr.argvs())
		}
		if call.Argv[0] == postlude {
			postludeRan = true
		}
	}
	if !postludeRan {
		t.Fatalf("the postlude must still run after a skip: %v", fr.argvs())
	}

	rec := readRecord(t, d)
	if rec.Status != contract.StatusOK || !rec.AgentSkipped {
		t.Fatalf("record = %+v, want ok with agent_skipped", rec)
	}
}

// Verifies ae434449cac9: a template that did not opt in ignores the signal —
// and says so: a warning names the missing agent_optional, the agent runs,
// the record carries no skip marker, and the consumed key still never
// reaches the agent's environment.
func TestSkipRequestIgnoredWithoutOptIn(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	fr.handle = skipPreludeHandler(t, d, prelude, "1", true)
	var logs bytes.Buffer
	b := New(ParseEnv(baseEnv(d, nil)), fr, baseEnv(d, nil), slog.New(slog.NewTextHandler(&logs, nil)))
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	agentRan := false
	for _, call := range fr.calls {
		if call.Argv[0] != "agent-cli" {
			continue
		}
		agentRan = true
		derived, leaked := false, false
		for _, kv := range call.Env {
			if strings.HasPrefix(kv, "DERIVED=") {
				derived = true
			}
			if strings.HasPrefix(kv, contract.SkipAgentKey+"=") {
				leaked = true
			}
		}
		if !derived {
			t.Fatal("ordinary sidecar values must still reach the agent env")
		}
		if leaked {
			t.Fatal("the skip key must be consumed even when the request is ignored")
		}
	}
	if !agentRan {
		t.Fatalf("without the opt-in the agent must run: %v", fr.argvs())
	}
	if !strings.Contains(logs.String(), "does not declare agent_optional") {
		t.Fatalf("the ignored request must be logged:\n%s", logs.String())
	}
	if rec := readRecord(t, d); rec.AgentSkipped {
		t.Fatalf("an ignored request must not mark the record: %+v", rec)
	}
}

// Verifies b880aa49b3b9: a skip value other than the contract "1" is a
// bundle contract error — the step fails at the prelude phase instead of
// guessing, opt-in or not.
func TestSkipRequestInvalidValueFails(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	fr.handle = skipPreludeHandler(t, d, prelude, "yes", false)
	b := newTestBox(t, d, map[string]string{contract.EnvAgentOptional: "1"}, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusFailed || rec.Error == nil || rec.Error.Reason != contract.ReasonBundleMalformed {
		t.Fatalf("record = %+v, want bundle-malformed", rec)
	}
	if !strings.Contains(rec.Error.Detail, contract.SkipAgentKey) {
		t.Fatalf("detail must name the key: %q", rec.Error.Detail)
	}
}

// Verifies ff8e85704b0a: the output contract does not relax for a skipped
// agent — required outputs unsatisfied fail missing-output, with the detail
// naming the skip so the diagnosis points at the prelude, not a silent
// agent. The failed record still carries the skip marker.
func TestSkipWithUnsatisfiedOutputsFailsLoudly(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	fr.handle = skipPreludeHandler(t, d, prelude, "1", false)
	schema := `{"result":{"type":"string","required":true}}`
	b := newTestBox(t, d, map[string]string{
		contract.EnvAgentOptional: "1",
		contract.EnvOutputSchema:  schema,
	}, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusFailed || rec.Error == nil || rec.Error.Reason != contract.ReasonMissingOutput {
		t.Fatalf("record = %+v, want missing-output", rec)
	}
	if !strings.Contains(rec.Error.Detail, "skipped by the prelude") {
		t.Fatalf("detail must name the skip: %q", rec.Error.Detail)
	}
	if !rec.AgentSkipped {
		t.Fatalf("the failed record must still mark the skip: %+v", rec)
	}
}

// Verifies ff8e85704b0a: a prelude that satisfies the output contract itself
// completes the whole step without agent or postlude involvement — the
// happy path of a deterministic step.
func TestSkipWithPreludeWrittenOutputs(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	prelude := writeHook(t, d, contract.HookPrelude)
	base := skipPreludeHandler(t, d, prelude, "1", false)
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		res, err := base(spec, stream)
		if spec.Argv[0] == prelude {
			writeOutput(t, d, `{"result":"landed"}`)
		}
		return res, err
	}
	schema := `{"result":{"type":"string","required":true}}`
	b := newTestBox(t, d, map[string]string{
		contract.EnvAgentOptional: "1",
		contract.EnvOutputSchema:  schema,
	}, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusOK || !rec.AgentSkipped || rec.Payload["result"] != "landed" {
		t.Fatalf("record = %+v, want ok, agent_skipped, prelude-written payload", rec)
	}
	if rec.Fallback {
		t.Fatalf("a written output.json is not the fallback: %+v", rec)
	}
}

// Verifies the env contract floor: FABER_AGENT_OPTIONAL accepts only the
// exact contract value "1" — anything else is an env-contract violation,
// never a silent opt-in.
func TestAgentOptionalEnvStrictValue(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	b := newTestBox(t, d, map[string]string{contract.EnvAgentOptional: "true"}, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusFailed || rec.Error == nil || rec.Error.Reason != contract.ReasonEnvContract {
		t.Fatalf("record = %+v, want env-contract", rec)
	}
	if !strings.Contains(rec.Error.Detail, contract.EnvAgentOptional) {
		t.Fatalf("detail must name the variable: %q", rec.Error.Detail)
	}
}
