package box

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
)

// Verifies ae434449cac9: the byte-for-byte guard on the built-in default
// profile — the prompt is the skill-activating slash command, the bundle body
// verbatim, and the clearly delimited optional trailer; the argv always
// carries the permission bypass (the sealed environment is the restriction)
// and pass-through flags only when set. The expectations are the exact bytes
// the pre-profile invoker emitted, so the profile mechanism can never drift
// the default dialect.
func TestInvocationPromptAndArgv(t *testing.T) {
	def := config.DefaultInvoke()
	tests := []struct {
		name       string
		inv        Invocation
		wantPrompt string
		wantArgv   []string
	}{
		{
			name:       "minimal",
			inv:        Invocation{CLI: "agent-cli", Profile: def, Skill: "skill-a", Body: "body\n"},
			wantPrompt: "/skill-a\n\nbody\n",
			wantArgv:   []string{"agent-cli", "-p", "/skill-a\n\nbody\n", "--permission-mode", "bypassPermissions"},
		},
		{
			name:       "all pass-throughs",
			inv:        Invocation{CLI: "agent-cli", Profile: def, Skill: "skill-a", Body: "body", Extra: "note", Model: "agent-model", Effort: "high", MaxBudget: "2.50"},
			wantPrompt: "/skill-a\n\nbody\n\nADDITIONAL INSTRUCTION: note",
			wantArgv: []string{
				"agent-cli", "-p", "/skill-a\n\nbody\n\nADDITIONAL INSTRUCTION: note",
				"--permission-mode", "bypassPermissions", "--model", "agent-model", "--effort", "high", "--max-budget-usd", "2.50",
			},
		},
		{
			name:       "model and effort only (the mandatory template pair)",
			inv:        Invocation{CLI: "agent-cli", Profile: def, Skill: "skill-a", Body: "body", Model: "agent-model", Effort: "low"},
			wantPrompt: "/skill-a\n\nbody",
			wantArgv: []string{
				"agent-cli", "-p", "/skill-a\n\nbody",
				"--permission-mode", "bypassPermissions", "--model", "agent-model", "--effort", "low",
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

// Verifies ae434449cac9: a non-default profile drives every expansion seam —
// subcommand tokens, a positional prompt (empty prompt flag), the skill as a
// flag pair with nothing in the prompt, an empty fixed tail, and a dropped
// effort pair (empty flag) though the value is set.
func TestInvocationNonDefaultProfile(t *testing.T) {
	inv := Invocation{
		CLI: "other-cli",
		Profile: config.ResolvedInvoke{
			Subcommand:     []string{"run", "--quiet"},
			PromptFlag:     "", // positional prompt
			SkillMode:      config.SkillModeFlag,
			SkillFlag:      "--recipe",
			PromptTemplate: "{body}{extra}",
			ModelFlag:      "--llm",
			EffortFlag:     "", // this harness has no effort knob
			BudgetFlag:     "--cost-cap",
		},
		Skill: "skill-a", Body: "body", Extra: "note",
		Model: "agent-model", Effort: "high", MaxBudget: "2.50",
	}
	wantPrompt := "body\n\nADDITIONAL INSTRUCTION: note"
	if got := inv.Prompt(); got != wantPrompt {
		t.Fatalf("prompt = %q, want %q", got, wantPrompt)
	}
	wantArgv := []string{
		"other-cli", "run", "--quiet", wantPrompt,
		"--recipe", "skill-a", "--llm", "agent-model", "--cost-cap", "2.50",
	}
	if got := inv.Argv(); fmt.Sprint(got) != fmt.Sprint(wantArgv) {
		t.Fatalf("argv = %q, want %q", got, wantArgv)
	}
}

// Verifies ae434449cac9: prompt expansion is injection-proof — placeholder
// literals inside the bundle body (or the operator note) survive verbatim,
// never re-expanded, because substituted text is not re-scanned.
func TestInvocationPromptNoReExpansion(t *testing.T) {
	inv := Invocation{
		CLI: "agent-cli", Profile: config.DefaultInvoke(), Skill: "skill-a",
		Body:  "uses {extra} and {skill} literally",
		Extra: "note with {body}",
	}
	want := "/skill-a\n\nuses {extra} and {skill} literally\n\nADDITIONAL INSTRUCTION: note with {body}"
	if got := inv.Prompt(); got != want {
		t.Fatalf("prompt = %q, want %q", got, want)
	}
}

// Verifies ae434449cac9: unset model/effort/budget emit no flags at all (the
// omission path serves direct sequencer invocations; engine runs always set
// model and effort from the template's mandatory fields).
func TestInvocationOmitsUnsetFlags(t *testing.T) {
	argv := Invocation{CLI: "agent-cli", Profile: config.DefaultInvoke(), Skill: "skill-a", Body: "b"}.Argv()
	joined := strings.Join(argv, " ")
	for _, flag := range []string{"--model", "--effort", "--max-budget-usd"} {
		if strings.Contains(joined, flag) {
			t.Fatalf("argv %q carries %s though unset", joined, flag)
		}
	}
}

// Verifies ae434449cac9: FABER_INVOKE_PROFILE drives the invocation end to
// end through the sequencer — present, the recorded agent argv follows the
// profile's dialect; absent, it is the default dialect; malformed JSON or a
// rule-breaking profile fails the env phase before any subprocess runs.
func TestInvokeProfileEnvContract(t *testing.T) {
	run := func(t *testing.T, profile string) (testDirs, *fakeRunner, int) {
		d := newTestDirs(t)
		fr := &fakeRunner{}
		fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
			if spec.Argv[0] == "agent-cli" {
				writeOutput(t, d, `{}`)
			}
			return CmdResult{}, nil
		}
		b := newTestBox(t, d, map[string]string{contract.EnvInvokeProfile: profile}, fr)
		return d, fr, Main(context.Background(), b)
	}
	agentCall := func(t *testing.T, fr *fakeRunner) []string {
		t.Helper()
		for _, c := range fr.calls {
			if c.Argv[0] == "agent-cli" {
				return c.Argv
			}
		}
		t.Fatal("agent never invoked")
		return nil
	}
	t.Run("profile dialect on the argv", func(t *testing.T) {
		_, fr, code := run(t, `{"subcommand":["run"],"skill_mode":"flag","skill_flag":"--recipe","prompt_template":"{body}{extra}"}`)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		argv := agentCall(t, fr)
		// [cli, subcommand, positional prompt, skill pair]; nothing else — the
		// absent JSON fields are concrete empties, never re-defaulted.
		if len(argv) != 5 || argv[1] != "run" || argv[3] != "--recipe" || argv[4] != "skill-a" {
			t.Fatalf("argv = %q, want [agent-cli run <prompt> --recipe skill-a]", argv)
		}
		if strings.Contains(argv[2], "/skill-a") {
			t.Fatalf("prompt %q carries the skill though the profile moved it to a flag", argv[2])
		}
	})
	t.Run("absent profile falls back to the default dialect", func(t *testing.T) {
		_, fr, code := run(t, "")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
		argv := agentCall(t, fr)
		joined := strings.Join(argv, " ")
		if argv[1] != "-p" || !strings.Contains(joined, "--permission-mode bypassPermissions") {
			t.Fatalf("argv = %q, want the default dialect", argv)
		}
	})
	t.Run("malformed JSON fails the env phase", func(t *testing.T) {
		d, fr, code := run(t, `{not json`)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if len(fr.calls) != 0 {
			t.Fatalf("no subprocess may run after an env-contract violation, got %v", fr.argvs())
		}
		if h := readHandoff(t, d); h.Phase != "env" || h.Reason != contract.ReasonEnvContract {
			t.Fatalf("handoff = %+v, want phase env reason env-contract", h)
		}
	})
	t.Run("rule-breaking profile fails the env phase", func(t *testing.T) {
		d, fr, code := run(t, `{"skill_mode":"prefix","prompt_template":"{body}"}`)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1", code)
		}
		if len(fr.calls) != 0 {
			t.Fatalf("no subprocess may run, got %v", fr.argvs())
		}
		rec := readRecord(t, d)
		for _, part := range []string{contract.EnvInvokeProfile, "prompt_template"} {
			if !strings.Contains(rec.Error.Detail, part) {
				t.Errorf("detail %q does not name %q", rec.Error.Detail, part)
			}
		}
	})
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
		b := newTestBox(t, d, map[string]string{
			"FABER_REMOTE_URL": "/gw/repo-a.git",
			"FABER_GIT_EMAIL":  "dev@example.com",
		}, fr)
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
