package config

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeExecutor struct {
	called bool
	ir     *IR
	params Params
	opts   RunOptions
	err    error
}

func (f *fakeExecutor) Execute(ctx context.Context, ir *IR, params Params, opts RunOptions, logger *slog.Logger) error {
	f.called = true
	f.ir, f.params, f.opts = ir, params, opts
	return f.err
}

type fakeBuilder struct{ built []string }

func (f *fakeBuilder) BuildImage(ctx context.Context, cfg *Config, template string, logger *slog.Logger) error {
	f.built = append(f.built, template)
	return nil
}

type fakeProver struct {
	called bool
	err    error
}

func (f *fakeProver) ProvePackages(ctx context.Context, cfg *Config, logger *slog.Logger) error {
	f.called = true
	return f.err
}

type fakeJournal struct{ header JournalHeader }

func (f fakeJournal) LoadHeader(runID string) (JournalHeader, error) { return f.header, nil }

// runCLI invokes the in-process CLI harness and captures exit code, stdout,
// and stderr.
func runCLI(t *testing.T, deps Deps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithDeps(args, &stdout, &stderr, deps)
	return code, stdout.String(), stderr.String()
}

// brokenReferencePath writes a one-defect mutation of the reference config
// (undeclared output field) to a temp file and returns its path.
func brokenReferencePath(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/reference.yaml")
	if err != nil {
		t.Fatal(err)
	}
	mutated := strings.Replace(string(data), `"${steps.implement.pr}"`, `"${steps.implement.prs}"`, 1)
	path := filepath.Join(t.TempDir(), "orchestrator.yaml")
	if err := os.WriteFile(path, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Verifies 67c77533453d: the CLI exit-code and output contract — no args is a
// usage error (exit 2), a pristine validate exits 0 silently, --emit-ir writes
// exactly the canonical IR bytes to stdout, a broken config exits 1 with the
// violations on stderr, and output is stable across runs.
func TestCLIContract(t *testing.T) {
	t.Run("no args", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{})
		if code != 2 || !strings.Contains(stderr, "usage: faber") {
			t.Fatalf("want exit 2 with usage, got %d, stderr: %s", code, stderr)
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "conjure")
		if code != 2 || !strings.Contains(stderr, `unknown command "conjure"`) {
			t.Fatalf("want exit 2, got %d, stderr: %s", code, stderr)
		}
	})

	t.Run("pristine validate exits 0 silently", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, Deps{}, "validate", "--config", "testdata/reference.yaml")
		if code != 0 {
			t.Fatalf("want exit 0, got %d, stderr: %s", code, stderr)
		}
		if stdout != "" {
			t.Fatalf("validate without --emit-ir writes nothing to stdout, got: %s", stdout)
		}
	})

	t.Run("emit-ir writes canonical bytes and nothing else to stdout", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, Deps{}, "validate",
			"--config", "testdata/reference.yaml", "--workflow", "task", "--emit-ir")
		if code != 0 {
			t.Fatalf("want exit 0, got %d, stderr: %s", code, stderr)
		}
		golden, err := os.ReadFile("testdata/reference_task.ir.json")
		if err != nil {
			t.Fatal(err)
		}
		if stdout != string(golden) {
			t.Fatal("--emit-ir stdout must match the golden IR byte-for-byte")
		}
		_, again, _ := runCLI(t, Deps{}, "validate",
			"--config", "testdata/reference.yaml", "--workflow", "task", "--emit-ir")
		if stdout != again {
			t.Fatal("--emit-ir output must be stable across runs")
		}
	})

	t.Run("broken config exits 1 with field-path violations", func(t *testing.T) {
		path := brokenReferencePath(t)
		code, stdout, stderr := runCLI(t, Deps{}, "validate", "--config", path, "--emit-ir")
		if code != 1 {
			t.Fatalf("want exit 1, got %d", code)
		}
		if !strings.Contains(stderr, "output field does not exist (did you mean pr?)") {
			t.Fatalf("violation missing from stderr: %s", stderr)
		}
		if stdout != "" {
			t.Fatal("no IR is emitted for an invalid config")
		}
	})

	t.Run("unknown workflow", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "validate", "--config", "testdata/reference.yaml", "--workflow", "ghost")
		if code != 1 || !strings.Contains(stderr, `unknown workflow "ghost"`) {
			t.Fatalf("want exit 1, got %d, stderr: %s", code, stderr)
		}
	})

	t.Run("package proof seam is called when wired", func(t *testing.T) {
		prover := &fakeProver{}
		code, _, _ := runCLI(t, Deps{Prover: prover}, "validate", "--config", "testdata/reference.yaml")
		if code != 0 || !prover.called {
			t.Fatalf("prover must be invoked by validate when wired (code %d, called %v)", code, prover.called)
		}
	})
}

// Verifies 67c77533453d and 255893ae16eb: run-entry param checking through the
// CLI — a missing required param is a hard error before anything is built or
// launched; unknown params list the declared interface; an empty string is a
// present value.
func TestCLIRunEntry(t *testing.T) {
	t.Run("missing required param launches nothing", func(t *testing.T) {
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Executor: exec}, "run", "task",
			"--config", "testdata/reference.yaml", "--param", "repo=sandbox")
		if code != 1 || !strings.Contains(stderr, "params.item: required param missing") {
			t.Fatalf("want exit 1 with missing-param error, got %d: %s", code, stderr)
		}
		if exec.called {
			t.Fatal("nothing may be launched when a required param is missing")
		}
	})

	t.Run("unknown param lists the declared interface", func(t *testing.T) {
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Executor: exec}, "run", "task",
			"--config", "testdata/reference.yaml",
			"--param", "repo=sandbox", "--param", "item=I-1", "--param", "surprise=x")
		if code != 1 || !strings.Contains(stderr, "unknown param (declared params: item, repo)") {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if exec.called {
			t.Fatal("executor must not run on a param violation")
		}
	})

	t.Run("empty string is presence, not truthiness", func(t *testing.T) {
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Executor: exec}, "run", "task",
			"--config", "testdata/reference.yaml", "--param", "repo=sandbox", "--param", "item=")
		if code != 0 || !exec.called {
			t.Fatalf("empty param value is accepted, got %d: %s", code, stderr)
		}
		if exec.params["item"].Value != "" {
			t.Fatalf("item must carry the empty string, got %+v", exec.params["item"])
		}
	})

	t.Run("executor seam not wired yields a structured error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "run", "task",
			"--config", "testdata/reference.yaml", "--param", "repo=sandbox", "--param", "item=I-1")
		if code != 1 || !strings.Contains(stderr, "requires the pipeline module") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("run options reach the executor", func(t *testing.T) {
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Executor: exec}, "run", "task",
			"--config", "testdata/reference.yaml", "--param", "repo=sandbox", "--param", "item=I-1",
			"--max-parallel", "3", "--budget", "tokens=100", "--metering", "./meter.yaml")
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if exec.opts.MaxParallel != 3 || exec.opts.Budgets["tokens"] != 100 || exec.opts.MeteringPath != "./meter.yaml" {
			t.Fatalf("run options lost: %+v", exec.opts)
		}
		if exec.ir == nil || exec.ir.Workflow != "task" {
			t.Fatalf("executor must receive the entry IR, got %+v", exec.ir)
		}
	})

	t.Run("workflows reachable via generate are validated for a run", func(t *testing.T) {
		// Break task's wiring; running epic (which only reaches task through
		// generate) must still fail validation.
		path := brokenReferencePath(t)
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Executor: exec}, "run", "epic",
			"--config", path, "--param", "repo=sandbox", "--param", "group=G-1")
		if code != 1 || !strings.Contains(stderr, "output field does not exist") {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if exec.called {
			t.Fatal("executor must not run when a reachable workflow is broken")
		}
	})

	t.Run("malformed param is a usage error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "run", "task",
			"--config", "testdata/reference.yaml", "--param", "noequals")
		if code != 2 || !strings.Contains(stderr, "expected k=v") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})
}

// Verifies 67c77533453d: the resume guard — resuming after the config changed
// (different IR hash) refuses with a mismatch message; --fresh proceeds; a
// matching hash resumes.
func TestCLIResumeGuard(t *testing.T) {
	goodHash, err := HashIR(desugarRef(t, "task"))
	if err != nil {
		t.Fatal(err)
	}
	header := JournalHeader{
		RunID:      "r-1",
		ConfigPath: "testdata/reference.yaml",
		Workflow:   "task",
		Params:     map[string]string{"repo": "sandbox", "item": "I-1"},
	}

	t.Run("journal seam not wired yields a structured error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "resume", "r-1")
		if code != 1 || !strings.Contains(stderr, "require the failure module") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("hash mismatch refuses", func(t *testing.T) {
		h := header
		h.IRHash = "0000000000000000"
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Journal: fakeJournal{h}, Executor: exec}, "resume", "r-1")
		if code != 1 || !strings.Contains(stderr, "config has changed since the run") {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if exec.called {
			t.Fatal("a mismatched journal must not execute")
		}
	})

	t.Run("fresh proceeds past the mismatch", func(t *testing.T) {
		h := header
		h.IRHash = "0000000000000000"
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Journal: fakeJournal{h}, Executor: exec}, "resume", "r-1", "--fresh")
		if code != 0 || !exec.called || exec.opts.Mode != "fresh" {
			t.Fatalf("--fresh must proceed with mode fresh, got %d (%+v): %s", code, exec.opts, stderr)
		}
	})

	t.Run("matching hash resumes", func(t *testing.T) {
		h := header
		h.IRHash = goodHash
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Journal: fakeJournal{h}, Executor: exec}, "resume", "r-1")
		if code != 0 || !exec.called || exec.opts.Mode != "resume" || exec.opts.RunID != "r-1" {
			t.Fatalf("got %d (%+v): %s", code, exec.opts, stderr)
		}
	})

	t.Run("interactive mode dispatches", func(t *testing.T) {
		h := header
		h.IRHash = goodHash
		exec := &fakeExecutor{}
		code, _, _ := runCLI(t, Deps{Journal: fakeJournal{h}, Executor: exec}, "resume", "r-1", "--interactive", "task/merge")
		if code != 0 || exec.opts.Mode != "interactive" || exec.opts.InteractiveStep != "task/merge" {
			t.Fatalf("got %d (%+v)", code, exec.opts)
		}
	})
}

// Verifies 67c77533453d: faber build dispatch — a structured error without the
// infra seam, and per-template builds with it.
func TestCLIBuildDispatch(t *testing.T) {
	t.Run("builder seam not wired yields a structured error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "build", "--config", "testdata/reference.yaml")
		if code != 1 || !strings.Contains(stderr, "require the infra module") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("builds every template, or one with --template", func(t *testing.T) {
		b := &fakeBuilder{}
		code, _, stderr := runCLI(t, Deps{Builder: b}, "build", "--config", "testdata/reference.yaml")
		if code != 0 || len(b.built) != 4 {
			t.Fatalf("want 4 templates built, got %v (%d): %s", b.built, code, stderr)
		}
		b = &fakeBuilder{}
		code, _, _ = runCLI(t, Deps{Builder: b}, "build", "--config", "testdata/reference.yaml", "--template", "review")
		if code != 0 || len(b.built) != 1 || b.built[0] != "review" {
			t.Fatalf("want [review], got %v", b.built)
		}
	})
}

// Verifies 67c77533453d: slog initialization — JSON for non-TTY, explicit
// --log-format beats auto-detection, child loggers carry component context,
// and unknown levels/formats are usage errors.
func TestCLILogging(t *testing.T) {
	t.Run("non-TTY auto is JSON", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := InitLogging("info", "auto", &buf)
		if err != nil {
			t.Fatal(err)
		}
		logger.With("component", "cli").Info("hello")
		if !strings.HasPrefix(buf.String(), "{") || !strings.Contains(buf.String(), `"component":"cli"`) {
			t.Fatalf("want JSON with component attr, got: %s", buf.String())
		}
	})

	t.Run("explicit format beats auto-detection", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := InitLogging("info", "text", &buf)
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("hello")
		if !strings.Contains(buf.String(), "level=INFO") {
			t.Fatalf("want text handler output, got: %s", buf.String())
		}
		buf.Reset()
		logger, err = InitLogging("info", "json", &buf)
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("hello")
		if !strings.HasPrefix(buf.String(), "{") {
			t.Fatalf("want JSON handler output, got: %s", buf.String())
		}
	})

	t.Run("level filtering", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := InitLogging("warn", "json", &buf)
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("quiet")
		logger.Warn("loud")
		if strings.Contains(buf.String(), "quiet") || !strings.Contains(buf.String(), "loud") {
			t.Fatalf("level filtering broken: %s", buf.String())
		}
	})

	t.Run("unknown level is a usage error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "validate", "--config", "testdata/reference.yaml", "--log-level", "chatty")
		if code != 2 || !strings.Contains(stderr, `unknown --log-level "chatty"`) {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})
}
