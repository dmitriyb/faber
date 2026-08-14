package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeRuns is a recording RunController.
type fakeRuns struct {
	resolve  map[string]string // ref -> id; absent refs error
	pausedID string
	pauseErr error
	pruned   []string
	pruneAll bool
	pruneErr error
}

func (f *fakeRuns) ResolveRunRef(ref string) (string, error) {
	if id, ok := f.resolve[ref]; ok {
		return id, nil
	}
	return "", errors.New("failure: no run with id or name " + ref)
}

func (f *fakeRuns) RequestPause(runID string) error {
	f.pausedID = runID
	return f.pauseErr
}

func (f *fakeRuns) PruneRuns(all bool) ([]string, error) {
	f.pruneAll = all
	return f.pruned, f.pruneErr
}

// Verifies 67c77533453d: the runs group is thin dispatch — the listing
// derives each state from the audit facts alone, pause resolves the
// reference before requesting, prune propagates --all, and the usage/
// operational error split holds.
func TestCLIRunsGroup(t *testing.T) {
	started := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	audits := []RunAudit{
		{RunID: "r-aborted", Complete: true, EndStatus: "aborted"},
		{RunID: "r-inc"},
		{RunID: "r-live", Live: true},
		{RunID: "r-paused", Complete: true, EndStatus: "paused", Name: "nightly", Workflow: "task", Started: started},
		{RunID: "r-settled", Complete: true, EndStatus: "settled"},
	}

	t.Run("list derives states from audit facts", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, Deps{Audit: fakeAudit{runs: audits}}, "runs")
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		for _, want := range []string{
			"r-live", "live",
			"r-inc", "incomplete",
			"r-paused", "nightly", "task", "paused", "2026-08-01T10:00:00Z",
			"r-settled", "settled",
			"r-aborted", "aborted",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("listing must contain %q:\n%s", want, stdout)
			}
		}
	})

	t.Run("list --json emits the same rows machine-readably", func(t *testing.T) {
		code, stdout, stderr := runCLI(t, Deps{Audit: fakeAudit{runs: audits}}, "runs", "--json")
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
			t.Fatalf("stdout must be a JSON array: %v\n%s", err, stdout)
		}
		if len(rows) != len(audits) {
			t.Fatalf("got %d rows, want %d", len(rows), len(audits))
		}
		states := map[string]string{}
		for _, r := range rows {
			states[r["run_id"].(string)], _ = r["state"].(string)
		}
		want := map[string]string{
			"r-live": "live", "r-inc": "incomplete", "r-paused": "paused",
			"r-settled": "settled", "r-aborted": "aborted",
		}
		for id, state := range want {
			if states[id] != state {
				t.Errorf("%s: state %q, want %q", id, states[id], state)
			}
		}
	})

	t.Run("empty store lists cleanly", func(t *testing.T) {
		code, stdout, _ := runCLI(t, Deps{Audit: fakeAudit{}}, "runs")
		if code != 0 || !strings.Contains(stdout, "no journaled runs") {
			t.Fatalf("got %d: %s", code, stdout)
		}
	})

	t.Run("unknown subcommand is a usage error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{Audit: fakeAudit{}}, "runs", "bogus")
		if code != 2 || !strings.Contains(stderr, "unknown subcommand") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("unwired audit seam is a structured error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{}, "runs")
		if code != 1 || !strings.Contains(stderr, "requires the failure module") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("pause resolves the reference then requests", func(t *testing.T) {
		runs := &fakeRuns{resolve: map[string]string{"nightly": "r-1"}}
		code, stdout, stderr := runCLI(t, Deps{Runs: runs}, "runs", "pause", "nightly")
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if runs.pausedID != "r-1" {
			t.Fatalf("pause must target the resolved id, got %q", runs.pausedID)
		}
		if !strings.Contains(stdout, "r-1") || !strings.Contains(stdout, "faber resume r-1") {
			t.Errorf("confirmation must name the run and the resume command:\n%s", stdout)
		}
	})

	t.Run("pause without a reference is a usage error", func(t *testing.T) {
		code, _, stderr := runCLI(t, Deps{Runs: &fakeRuns{}}, "runs", "pause")
		if code != 2 || !strings.Contains(stderr, "usage: faber runs pause") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("pause refusal is operational", func(t *testing.T) {
		runs := &fakeRuns{
			resolve:  map[string]string{"r-1": "r-1"},
			pauseErr: errors.New("failure: pause run r-1: the run is not live"),
		}
		code, _, stderr := runCLI(t, Deps{Runs: runs}, "runs", "pause", "r-1")
		if code != 1 || !strings.Contains(stderr, "not live") {
			t.Fatalf("got %d: %s", code, stderr)
		}
	})

	t.Run("prune reports removals and propagates --all", func(t *testing.T) {
		runs := &fakeRuns{pruned: []string{"r-old-1", "r-old-2"}}
		code, stdout, stderr := runCLI(t, Deps{Runs: runs}, "runs", "prune", "--all")
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if !runs.pruneAll {
			t.Fatal("--all must reach the controller")
		}
		if !strings.Contains(stdout, "removed 2 run(s)") || !strings.Contains(stdout, "r-old-1") {
			t.Errorf("prune must name what it removed:\n%s", stdout)
		}

		runs = &fakeRuns{}
		code, stdout, _ = runCLI(t, Deps{Runs: runs}, "runs", "prune")
		if code != 0 || !strings.Contains(stdout, "nothing to remove") || runs.pruneAll {
			t.Fatalf("empty prune: %d, all=%v: %s", code, runs.pruneAll, stdout)
		}
	})

	t.Run("prune with an argument is a usage error", func(t *testing.T) {
		runs := &fakeRuns{pruned: []string{"r-x"}}
		code, _, stderr := runCLI(t, Deps{Runs: runs}, "runs", "prune", "extra")
		if code != 2 || !strings.Contains(stderr, "unexpected argument") {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if runs.pruneAll || len(runs.pruned) != 1 {
			// The controller must not have been driven; pruned is the canned
			// return, untouched.
			t.Fatal("a usage error must not reach the controller")
		}
	})

	t.Run("listing strips terminal controls from journal-derived text", func(t *testing.T) {
		hostile := []RunAudit{{RunID: "r-1", Complete: true, EndStatus: "settled", Name: "bad\x1b[2Jname"}}
		code, stdout, _ := runCLI(t, Deps{Audit: fakeAudit{runs: hostile}}, "runs")
		if code != 0 {
			t.Fatalf("got %d", code)
		}
		if strings.Contains(stdout, "\x1b") {
			t.Fatalf("escape byte must not reach the terminal:\n%q", stdout)
		}
	})
}

// Verifies 67c77533453d: --name threads into RunOptions, resume accepts a
// name-form reference through the RunController seam (and falls back to the
// raw id when the seam is unwired), and a typed exit-4 error maps through
// the generic contract.
func TestCLIRunNamesAndPauseExit(t *testing.T) {
	t.Run("run --name reaches the executor options", func(t *testing.T) {
		exec := &fakeExecutor{}
		code, _, stderr := runCLI(t, Deps{Executor: exec}, "run", "task",
			"--config", "testdata/reference.yaml", "--name", "nightly",
			"--param", "repo=sandbox", "--param", "item=I-1")
		if code != 0 {
			t.Fatalf("got %d: %s", code, stderr)
		}
		if exec.opts.Name != "nightly" {
			t.Fatalf("RunOptions.Name = %q, want nightly", exec.opts.Name)
		}
	})

	t.Run("resume resolves a name to its run id", func(t *testing.T) {
		goodHash, err := HashIR(desugarRef(t, "task"))
		if err != nil {
			t.Fatal(err)
		}
		header := JournalHeader{
			RunID:      "r-1",
			ConfigPath: "testdata/reference.yaml",
			Workflow:   "task",
			Params:     map[string]string{"repo": "sandbox", "item": "I-1"},
			Format:     1,
			IRVersion:  IRVersion,
			IRHash:     goodHash,
		}
		exec := &fakeExecutor{}
		runs := &fakeRuns{resolve: map[string]string{"nightly": "r-1"}}
		code, _, stderr := runCLI(t, Deps{Journal: fakeJournal{header}, Executor: exec, Runs: runs},
			"resume", "nightly")
		if code != 0 || exec.opts.RunID != "r-1" {
			t.Fatalf("got %d (RunID %q): %s", code, exec.opts.RunID, stderr)
		}

		// An unresolvable reference refuses before any guard runs.
		exec = &fakeExecutor{}
		code, _, stderr = runCLI(t, Deps{Journal: fakeJournal{header}, Executor: exec, Runs: runs},
			"resume", "ghost")
		if code != 1 || exec.called {
			t.Fatalf("unknown ref must refuse without executing, got %d: %s", code, stderr)
		}

		// With no RunController wired the positional is used as an id.
		exec = &fakeExecutor{}
		code, _, stderr = runCLI(t, Deps{Journal: fakeJournal{header}, Executor: exec}, "resume", "r-1")
		if code != 0 || exec.opts.RunID != "r-1" {
			t.Fatalf("got %d (RunID %q): %s", code, exec.opts.RunID, stderr)
		}

		// --fresh writes a brand-new header; the journaled name rides along
		// so a named run stays named across the restart.
		named := header
		named.Name = "nightly"
		exec = &fakeExecutor{}
		code, _, stderr = runCLI(t, Deps{Journal: fakeJournal{named}, Executor: exec}, "resume", "r-1", "--fresh")
		if code != 0 || exec.opts.Name != "nightly" {
			t.Fatalf("fresh restart must inherit the name, got %d (Name %q): %s", code, exec.opts.Name, stderr)
		}
	})

	t.Run("paused run exits 4 through the generic ExitCode mapping", func(t *testing.T) {
		exec := &fakeExecutor{err: &cliError{code: 4, err: errors.New("pipeline: run r-1 paused on request")}}
		code, _, stderr := runCLI(t, Deps{Executor: exec}, "run", "task",
			"--config", "testdata/reference.yaml", "--param", "repo=sandbox", "--param", "item=I-1")
		if code != 4 || !strings.Contains(stderr, "paused") {
			t.Fatalf("paused run must exit 4 with the message on stderr, got %d: %s", code, stderr)
		}
	})
}
