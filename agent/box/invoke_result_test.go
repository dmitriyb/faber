package box

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
)

// Verifies ae434449cac9: the prompt is the skill-activating slash command,
// the bundle body verbatim, and the clearly delimited optional trailer; the
// argv always carries the permission bypass (the sealed environment is the
// restriction) and pass-through flags only when set.
func TestInvocationPromptAndArgv(t *testing.T) {
	tests := []struct {
		name       string
		inv        Invocation
		wantPrompt string
		wantArgv   []string
	}{
		{
			name:       "minimal",
			inv:        Invocation{CLI: "agent-cli", Skill: "skill-a", Body: "body\n"},
			wantPrompt: "/skill-a\n\nbody\n",
			wantArgv:   []string{"agent-cli", "-p", "/skill-a\n\nbody\n", "--permission-mode", "bypassPermissions"},
		},
		{
			name:       "all pass-throughs",
			inv:        Invocation{CLI: "agent-cli", Skill: "skill-a", Body: "body", Extra: "note", Effort: "high", MaxBudget: "2.50"},
			wantPrompt: "/skill-a\n\nbody\n\nADDITIONAL INSTRUCTION: note",
			wantArgv: []string{
				"agent-cli", "-p", "/skill-a\n\nbody\n\nADDITIONAL INSTRUCTION: note",
				"--permission-mode", "bypassPermissions", "--effort", "high", "--max-budget-usd", "2.50",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.inv.Prompt(); got != tt.wantPrompt {
				t.Fatalf("prompt = %q, want %q", got, tt.wantPrompt)
			}
			if got := tt.inv.Argv(); fmt.Sprint(got) != fmt.Sprint(tt.wantArgv) {
				t.Fatalf("argv = %q, want %q", got, tt.wantArgv)
			}
		})
	}
}

// Verifies ae434449cac9: unset effort/budget emit no flags at all.
func TestInvocationOmitsUnsetFlags(t *testing.T) {
	argv := Invocation{CLI: "agent-cli", Skill: "skill-a", Body: "b"}.Argv()
	joined := strings.Join(argv, " ")
	for _, flag := range []string{"--effort", "--max-budget-usd"} {
		if strings.Contains(joined, flag) {
			t.Fatalf("argv %q carries %s though unset", joined, flag)
		}
	}
}

// Verifies ae434449cac9: the agent's child environment is the box
// environment plus the bundle's sidecar values, so anything the prelude
// derived is visible to the skill.
func TestAgentEnvIncludesBundleSidecar(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	var agentCall *CmdSpec
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			c := spec
			agentCall = &c
			writeOutput(t, d, `{}`)
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, nil, fr)
	if err := b.checkEnv(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Workdir = d.workspace
	b.Bundle = &Bundle{Doc: "body", Env: map[string]string{"BRANCH": "t-1", "REF": "r-9"}}
	if err := b.runAgent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agentCall == nil {
		t.Fatal("agent never invoked")
	}
	if envLookup(agentCall.Env, "BRANCH") != "t-1" || envLookup(agentCall.Env, "REF") != "r-9" {
		t.Fatalf("agent env misses sidecar values")
	}
	if agentCall.Dir != d.workspace {
		t.Fatalf("agent cwd = %q, want workspace", agentCall.Dir)
	}
}

// Verifies ae434449cac9: a nonzero agent exit takes the fail-stop path with
// phase agent and the exit code; the result phase's extraction never runs on
// the stale output file.
func TestAgentCrashFailStops(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{"stale": true}`)
			return CmdResult{ExitCode: 17, StderrTail: []byte("boom\n")}, nil
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, nil, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatal("want failure exit")
	}
	h := readHandoff(t, d)
	if h.Phase != "agent" || h.Reason != contract.ReasonAgentFailed || h.ExitCode != 17 {
		t.Fatalf("handoff = %+v", h)
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusFailed || rec.Payload != nil {
		t.Fatalf("stale output must not be extracted, record = %+v", rec)
	}
}

// Verifies ff8e85704b0a: a missing output file after a successful agent
// phase yields the engine-written fallback record — ok with an empty payload
// under an all-optional schema, missing-output under a required one.
func TestFallbackRecord(t *testing.T) {
	schemaOptional := `{"note": {"type": "string"}}`
	schemaRequired := `{"note": {"type": "string", "required": true}}`
	run := func(t *testing.T, schema string) (contract.Result, int) {
		d := newTestDirs(t)
		fr := &fakeRunner{} // agent exits 0, writes nothing
		b := newTestBox(t, d, map[string]string{contract.EnvOutputSchema: schema}, fr)
		code := Main(context.Background(), b)
		return readRecord(t, d), code
	}
	t.Run("all optional schema tolerates a quiet agent", func(t *testing.T) {
		rec, code := run(t, schemaOptional)
		if code != 0 || rec.Status != contract.StatusOK || !rec.Fallback || len(rec.Payload) != 0 {
			t.Fatalf("code=%d record=%+v, want ok fallback empty payload", code, rec)
		}
	})
	t.Run("required output converts the fallback into missing-output", func(t *testing.T) {
		rec, code := run(t, schemaRequired)
		if code != 1 || rec.Status != contract.StatusFailed || rec.Error.Reason != contract.ReasonMissingOutput {
			t.Fatalf("code=%d record=%+v, want failed missing-output", code, rec)
		}
	})
}

// Verifies ff8e85704b0a: schema violations are collected — a wrong-typed
// field and an out-of-enum value are both listed under reason output-schema.
func TestSchemaViolationsCollected(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{"count": "not-int", "verdict": "maybe"}`)
		}
		return CmdResult{}, nil
	}
	schema := `{"count": {"type": "int", "required": true}, "verdict": {"type": "string", "enum": ["ok", "changes"], "required": true}}`
	b := newTestBox(t, d, map[string]string{contract.EnvOutputSchema: schema}, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatal("want failure exit")
	}
	rec := readRecord(t, d)
	if rec.Error.Reason != contract.ReasonOutputSchema {
		t.Fatalf("reason = %q", rec.Error.Reason)
	}
	for _, part := range []string{"count", "verdict"} {
		if !strings.Contains(rec.Error.Detail, part) {
			t.Errorf("detail %q misses violation for %q", rec.Error.Detail, part)
		}
	}
}

// Verifies ff8e85704b0a and f1ce19e94daa (first pass): an extra undeclared
// field alone does not fail — it stays in the record's payload but is marked
// unthreaded; typed JSON plus repo state are the only first-pass outputs.
func TestExtraFieldsUnthreadedNotFailed(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{"verdict": "ok", "surplus": 1}`)
		}
		return CmdResult{}, nil
	}
	schema := `{"verdict": {"type": "string", "required": true}}`
	b := newTestBox(t, d, map[string]string{contract.EnvOutputSchema: schema}, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatal("extra fields alone must not fail the attempt")
	}
	rec := readRecord(t, d)
	if rec.Payload["surplus"] != float64(1) {
		t.Fatal("extra field must be preserved in the record")
	}
	if fmt.Sprint(rec.Unthreaded) != "[surplus]" {
		t.Fatalf("unthreaded = %v", rec.Unthreaded)
	}
}

// Verifies ff8e85704b0a: an unfavorable payload value is an ok result —
// conditions, not failure semantics, react to the verdict.
func TestUnfavorableIsNotFailure(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{"verdict": "changes"}`)
		}
		return CmdResult{}, nil
	}
	schema := `{"verdict": {"type": "string", "enum": ["ok", "changes"], "required": true}}`
	b := newTestBox(t, d, map[string]string{contract.EnvOutputSchema: schema}, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatal("an unfavorable verdict must not fail the attempt")
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusOK || rec.Payload["verdict"] != "changes" {
		t.Fatalf("record = %+v", rec)
	}
}

// Verifies ff8e85704b0a: a declared BRANCH side-effect is verified against
// the gateway after extraction — a schema-valid payload claiming a push the
// gateway never accepted becomes side-effect-unverified.
func TestDeclaredSideEffectVerification(t *testing.T) {
	run := func(t *testing.T, lsRemoteExit int) (contract.Result, int, *fakeRunner) {
		d := newTestDirs(t)
		fr := &fakeRunner{}
		fr.handle = oneKeyHandler(func(spec CmdSpec, stream bool) (CmdResult, error) {
			switch spec.Argv[0] {
			case filepath.Join(d.hooks, contract.HookPrelude):
				writeFile(t, d.bundle, contract.ContextDoc, "body\n")
				writeFile(t, d.bundle, contract.BundleEnvFile, "BRANCH=t-1\n")
			case "agent-cli":
				writeOutput(t, d, `{}`)
			}
			return CmdResult{}, nil
		})
		base := fr.handle
		fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
			if spec.Argv[0] == "git" && spec.Argv[1] == "ls-remote" {
				return CmdResult{ExitCode: lsRemoteExit}, nil
			}
			return base(spec, stream)
		}
		writeHook(t, d, contract.HookPrelude)
		b := newTestBox(t, d, map[string]string{"FABER_REMOTE_URL": "/gw/repo-a.git"}, fr)
		code := Main(context.Background(), b)
		return readRecord(t, d), code, fr
	}
	t.Run("branch exists on the gateway", func(t *testing.T) {
		rec, code, fr := run(t, 0)
		if code != 0 || rec.Status != contract.StatusOK {
			t.Fatalf("code=%d record=%+v", code, rec)
		}
		want := "git ls-remote --exit-code origin refs/heads/t-1"
		found := false
		for _, c := range fr.calls {
			if strings.Join(c.Argv, " ") == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("verification never ran %q", want)
		}
	})
	t.Run("branch missing fails despite a valid payload", func(t *testing.T) {
		rec, code, _ := run(t, 2)
		if code != 1 || rec.Error == nil || rec.Error.Reason != contract.ReasonSideEffectUnverified {
			t.Fatalf("code=%d record=%+v, want side-effect-unverified", code, rec)
		}
	})
}

// Verifies ff8e85704b0a: unparseable output is an output-schema failure, not
// a crash or a silent fallback.
func TestGarbageOutputFile(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `not json at all`)
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, nil, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatal("want failure exit")
	}
	rec := readRecord(t, d)
	if rec.Error.Reason != contract.ReasonOutputSchema {
		t.Fatalf("reason = %q, want output-schema", rec.Error.Reason)
	}
}

// Verifies ff8e85704b0a: the attempt record echoes FABER_ATTEMPT and carries
// the sequencer's phase clocks.
func TestRecordAttemptAndTiming(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{}`)
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, map[string]string{contract.EnvAttempt: "3"}, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatal("want success")
	}
	rec := readRecord(t, d)
	if rec.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", rec.Attempt)
	}
	for _, phase := range []string{"env", "agent"} {
		if _, ok := rec.Timing[phase]; !ok {
			t.Fatalf("timing misses phase %q: %v", phase, rec.Timing)
		}
	}
}
