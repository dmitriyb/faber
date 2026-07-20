package failure

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/config"
)

// smallIR is a minimal deterministic IR (nodes/edges pre-sorted the way the
// desugarer emits them) for resume-compatibility checks.
func smallIR(extraNode bool) *config.IR {
	ir := &config.IR{
		IRVersion: 1,
		Workflow:  "main",
		Nodes: []config.Node{
			{ID: "task/a", Kind: config.KindAgent, Bindings: map[string]config.BindingDesc{}},
			{ID: "task/b", Kind: config.KindAgent, Bindings: map[string]config.BindingDesc{}},
			{ID: "task/d", Kind: config.KindAgent, Bindings: map[string]config.BindingDesc{}},
		},
		Edges: []config.Edge{
			{From: "task/a", FromPort: "field", To: "task/b", ToPort: "in"},
			{From: "task/b", FromPort: "field", To: "task/d", ToPort: "in"},
		},
	}
	if extraNode {
		ir.Nodes = append(ir.Nodes, config.Node{ID: "task/z", Kind: config.KindAgent, Bindings: map[string]config.BindingDesc{}})
	}
	return ir
}

func mustHashIR(t *testing.T, ir *config.IR) string {
	t.Helper()
	h, err := config.HashIR(ir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// journaledRun seeds a store with one run whose journal holds the given
// records under a header matching smallIR(false) and params.
func journaledRun(t *testing.T, runID string, params map[string]string, records ...ResultRecord) *Store {
	t.Helper()
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(Header{
		RunID:      runID,
		ConfigPath: "orchestrator.yaml",
		Workflow:   "main",
		Params:     params,
		IRHash:     mustHashIR(t, smallIR(false)),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records {
		if err := j.AppendResult(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return store
}

// Verifies 87f006277d2c: mid-run kill and resume — the journal has a's record
// only (b was in flight and left no record, which needed none). On resume
// with the identical config, a hits by (step-id, input-hash) and its payload
// is reused for downstream threading; b and everything after read as absent
// and run.
func TestResumeSkipsJournaledHits(t *testing.T) {
	params := map[string]string{"target": "v"}
	aInputs := map[string]any{"slot": "value"}
	aHash, err := InputHash(aInputs, "template-one", "image:tag")
	if err != nil {
		t.Fatal(err)
	}
	store := journaledRun(t, "run-1", params,
		ResultRecord{StepID: "task/a", InputHash: aHash, Result: okResult(`{"field":"a-out"}`)},
	)

	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Journal.Close()

	res, hit, err := seed.Lookup("task/a", aInputs, "template-one", "image:tag")
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("unchanged completed step must be skipped")
	}
	if string(res.Payload) != `{"field":"a-out"}` {
		t.Fatalf("skipped step's payload must be reused for threading, got %s", res.Payload)
	}
	// b never journaled: absent ⇒ run it. Its inputs include a's reused payload.
	if _, hit, err := seed.Lookup("task/b", map[string]any{"in": "a-out"}, "template-one", "image:tag"); err != nil || hit {
		t.Fatalf("absent step must run (hit=%v err=%v)", hit, err)
	}
	// The reopened journal accepts the re-run's fresh record.
	if err := seed.Journal.AppendResult(ResultRecord{StepID: "task/b", InputHash: "hash-b", Result: okResult(`{"field":"b-out"}`)}); err != nil {
		t.Fatal(err)
	}
}

// Verifies 87f006277d2c: reuse is exact, not fuzzy — a journaled hit with a
// different input-hash (an upstream output changed, the template was edited,
// or the image was rebuilt) is a miss, and a journaled failed record is never
// skipped.
func TestResumeReRunsChangedOrFailed(t *testing.T) {
	params := map[string]string{}
	inputs := map[string]any{"slot": "old"}
	oldHash, err := InputHash(inputs, "template-one", "image:tag")
	if err != nil {
		t.Fatal(err)
	}
	failedHash, err := InputHash(inputs, "template-two", "image:tag")
	if err != nil {
		t.Fatal(err)
	}
	store := journaledRun(t, "run-1", params,
		ResultRecord{StepID: "task/a", InputHash: oldHash, Result: okResult(`{"field":1}`)},
		ResultRecord{StepID: "task/b", InputHash: failedHash, Result: failedResult(ReasonAgent, "died")},
	)
	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Journal.Close()

	// Same step, changed slot value ⇒ different hash ⇒ re-run.
	if _, hit, _ := seed.Lookup("task/a", map[string]any{"slot": "new"}, "template-one", "image:tag"); hit {
		t.Fatal("changed inputs must invalidate the journal hit")
	}
	// Unchanged ⇒ skip.
	if _, hit, _ := seed.Lookup("task/a", inputs, "template-one", "image:tag"); !hit {
		t.Fatal("unchanged step must hit")
	}
	// Failed record ⇒ run it, even on exact key match.
	if _, hit, _ := seed.Lookup("task/b", inputs, "template-two", "image:tag"); hit {
		t.Fatal("a failed record is re-run, never skipped")
	}
}

// Verifies 87f006277d2c: resume refuses drift before trusting any skip
// decision — a changed graph (IR hash) or changed params fails with an
// explanation that points at --fresh; nothing is looked up.
func TestResumeRefusesDrift(t *testing.T) {
	params := map[string]string{"target": "v"}
	store := journaledRun(t, "run-1", params)

	_, err := store.Resume(smallIR(true), "run-1", params) // config grew a step
	if err == nil || !strings.Contains(err.Error(), "IR hash mismatch") || !strings.Contains(err.Error(), "--fresh") {
		t.Fatalf("want IR-hash refusal pointing at --fresh, got %v", err)
	}

	_, err = store.Resume(smallIR(false), "run-1", map[string]string{"target": "changed"})
	if err == nil || !strings.Contains(err.Error(), "params") || !strings.Contains(err.Error(), "--fresh") {
		t.Fatalf("want param refusal pointing at --fresh, got %v", err)
	}

	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatalf("matching IR and params must resume: %v", err)
	}
	seed.Journal.Close()
}

// Verifies 87f006277d2c: fresh (--fresh) ignores the journal — a new run
// id and journal, an empty prior map so every step runs, and the old journal
// left byte-for-byte untouched as a record of the abandoned run.
func TestFreshIgnoresJournal(t *testing.T) {
	params := map[string]string{}
	inputs := map[string]any{"slot": "v"}
	h, err := InputHash(inputs, "template-one", "image:tag")
	if err != nil {
		t.Fatal(err)
	}
	store := journaledRun(t, "run-1", params,
		ResultRecord{StepID: "task/a", InputHash: h, Result: okResult(`{"field":1}`)},
	)
	oldPath := filepath.Join(store.RunDir("run-1"), "journal.jsonl")
	oldBytes, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}

	seed, err := store.Fresh(Header{RunID: NewRunID(), Workflow: "main", IRHash: mustHashIR(t, smallIR(false))})
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Journal.Close()
	if seed.Header.RunID == "run-1" {
		t.Fatal("fresh must mint a new run id")
	}
	if _, hit, _ := seed.Lookup("task/a", inputs, "template-one", "image:tag"); hit {
		t.Fatal("fresh must not consult the old journal")
	}
	newBytes, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(newBytes) != string(oldBytes) {
		t.Fatal("fresh must leave the old journal untouched")
	}
}

// fakeReentry records the interactive target it was handed.
type fakeReentry struct {
	target *InteractiveTarget
}

func (f *fakeReentry) Reenter(_ context.Context, t InteractiveTarget) error {
	f.target = &t
	return nil
}

// Verifies 87f006277d2c: interactive re-entry hands the box-reconstruction
// seam the failed step's exact record — including the handoff pointer
// resolved under the run dir — and appends nothing to the journal; it refuses
// a step that settled ok or never settled, naming the step's actual state.
func TestInteractiveReentry(t *testing.T) {
	params := map[string]string{}
	failed := failedResult(ReasonAgent, "the agent phase died")
	failed.Error.Handoff = "handoff/task-b"
	store := journaledRun(t, "run-1", params,
		ResultRecord{StepID: "task/a", InputHash: "hash-a", Result: okResult(`{"field":1}`)},
		ResultRecord{StepID: "task/b", InputHash: "hash-b", Result: failed},
	)
	journalPath := filepath.Join(store.RunDir("run-1"), "journal.jsonl")
	before, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	// Refused: settled ok, naming the state.
	err = store.Interactive(t.Context(), "run-1", "task/a", &fakeReentry{})
	if err == nil || !strings.Contains(err.Error(), `settled "ok"`) {
		t.Fatalf("want refusal naming the ok state, got %v", err)
	}
	// Refused: never settled.
	err = store.Interactive(t.Context(), "run-1", "task/z", &fakeReentry{})
	if err == nil || !strings.Contains(err.Error(), "no journal record") {
		t.Fatalf("want refusal for an absent step, got %v", err)
	}

	// Failed step: the seam gets the record and the resolvable handoff path.
	re := &fakeReentry{}
	if err := store.Interactive(t.Context(), "run-1", "task/b", re); err != nil {
		t.Fatal(err)
	}
	if re.target == nil || re.target.StepID != "task/b" {
		t.Fatalf("reentry target missing: %+v", re.target)
	}
	if re.target.Record.Result.Error.Reason != ReasonAgent {
		t.Fatalf("reentry must carry the failed record: %+v", re.target.Record)
	}
	hp, ok := re.target.HandoffPath()
	if !ok || hp != filepath.Join(store.RunDir("run-1"), "handoff/task-b") {
		t.Fatalf("handoff pointer must resolve under the run dir, got %q ok=%v", hp, ok)
	}

	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("interactive session must write nothing to the journal")
	}
}

// Verifies 87f006277d2c: a failed generate item is a normal failure plus
// journal entry, not a special partial-run state — on resume the completed
// instances hit by hash and skip, the failed item misses and re-runs, and an
// item the source no longer emits is simply never looked up.
func TestFailedGenerateItemIsOrdinary(t *testing.T) {
	params := map[string]string{}
	itemInputs := func(n string) map[string]any { return map[string]any{"item": n} }
	hash := func(n string) string {
		h, err := InputHash(itemInputs(n), "template-one", "image:tag")
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	store := journaledRun(t, "run-1", params,
		ResultRecord{StepID: "fan/items@0/work", InputHash: hash("one"), Result: okResult(`{"field":"one"}`)},
		ResultRecord{StepID: "fan/items@1/work", InputHash: hash("two"), Result: failedResult(ReasonAgent, "item two died")},
		ResultRecord{StepID: "fan/items@2/work", InputHash: hash("three"), Result: okResult(`{"field":"three"}`)},
	)
	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Journal.Close()

	if _, hit, _ := seed.Lookup("fan/items@0/work", itemInputs("one"), "template-one", "image:tag"); !hit {
		t.Fatal("completed sibling must skip")
	}
	if _, hit, _ := seed.Lookup("fan/items@2/work", itemInputs("three"), "template-one", "image:tag"); !hit {
		t.Fatal("completed sibling must skip")
	}
	if _, hit, _ := seed.Lookup("fan/items@1/work", itemInputs("two"), "template-one", "image:tag"); hit {
		t.Fatal("the failed item must re-run")
	}
}

// Verifies 87f006277d2c: loop exhaustion is an ordinary failure record with
// reason loop-exhausted — resume treats it like any failed step (re-enters
// the chain, no special casing anywhere in journal or lookup).
func TestLoopExhaustionIsOrdinary(t *testing.T) {
	params := map[string]string{}
	inputs := map[string]any{"in": "v"}
	h, err := InputHash(inputs, "template-one", "image:tag")
	if err != nil {
		t.Fatal(err)
	}
	exhausted := failedResult(ReasonLoopExhausted, "3 iterations without settling")
	store := journaledRun(t, "run-1", params,
		ResultRecord{StepID: "task/cycle/select", InputHash: h, Result: exhausted},
	)
	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Journal.Close()
	if _, hit, _ := seed.Lookup("task/cycle/select", inputs, "template-one", "image:tag"); hit {
		t.Fatal("an exhausted loop is a failed step: it re-runs")
	}
	rp, err := store.Load("run-1")
	if err != nil {
		t.Fatal(err)
	}
	rec := rp.LastByStep["task/cycle/select"]
	if rec.Result.Error.Reason != ReasonLoopExhausted {
		t.Fatalf("exhaustion journals as a normal failure record, got %+v", rec.Result)
	}
}

// Verifies bff0f92afc29 and 87f006277d2c: crash mid-append then resume — the
// torn tail is dropped on Load AND neutralized on Reopen, so a post-resume
// append starts on a fresh line. Without the repair the new record would
// merge onto the fragment: first silently dropped as "torn tail", then, once
// another record follows, every Load would hard-error (run unresumable).
func TestReopenAfterTornTailRepairs(t *testing.T) {
	params := map[string]string{}
	inputs := map[string]any{"slot": "v"}
	h, err := InputHash(inputs, "template-one", "image:tag")
	if err != nil {
		t.Fatal(err)
	}
	store := journaledRun(t, "run-1", params,
		ResultRecord{StepID: "task/a", InputHash: h, Result: okResult(`{"field":"a-out"}`)},
	)
	// Crash mid-append: a Sync'd prefix of task/b's record, no trailing newline.
	journalPath := filepath.Join(store.RunDir("run-1"), "journal.jsonl")
	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"result","step_id":"task/b","input_ha`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Journal.Close()
	// The re-run of b settles and appends through the reopened journal.
	bRec := ResultRecord{StepID: "task/b", InputHash: "hash-b", Result: okResult(`{"field":"b-out"}`)}
	if err := seed.Journal.AppendResult(bRec); err != nil {
		t.Fatal(err)
	}
	if err := seed.Journal.AppendCost(CostRecord{StepID: "task/b", InputHash: "hash-b", Cost: json.RawMessage(`{"unit":1}`)}); err != nil {
		t.Fatal(err)
	}

	// A second Load (the next resume) must succeed and retain both records.
	rp, err := store.Load("run-1")
	if err != nil {
		t.Fatalf("journal corrupted by post-torn-tail append: %v", err)
	}
	if _, ok := rp.Results[Key{"task/a", h}]; !ok {
		t.Fatal("a's intact record lost")
	}
	got, ok := rp.Results[Key{"task/b", "hash-b"}]
	if !ok {
		t.Fatal("record appended after torn-tail repair was dropped on replay")
	}
	if string(got.Result.Payload) != `{"field":"b-out"}` {
		t.Fatalf("b's payload mangled: %s", got.Result.Payload)
	}
	if len(rp.Costs) != 1 {
		t.Fatalf("cost record after repair lost: %d", len(rp.Costs))
	}
}

// Verifies bff0f92afc29: resume replays journaled cost records into the seed
// (RunSeed.Costs) so the executor folds prior spend into the budget ledger —
// the fold arch_journal.md promises; fresh seeds carry none.
func TestResumeSeedsJournaledCosts(t *testing.T) {
	params := map[string]string{}
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(Header{RunID: "run-1", Workflow: "main", Params: params,
		IRHash: mustHashIR(t, smallIR(false))})
	if err != nil {
		t.Fatal(err)
	}
	if err := j.AppendCost(CostRecord{StepID: "task/a", InputHash: "h",
		Cost: json.RawMessage(`[{"unit":"tokens","amount":5}]`)}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	seed, err := store.Resume(smallIR(false), "run-1", params)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Journal.Close()
	if len(seed.Costs) != 1 || seed.Costs[0].StepID != "task/a" {
		t.Fatalf("resume must seed journaled cost records, got %+v", seed.Costs)
	}

	fresh, err := store.Fresh(Header{RunID: NewRunID(), Workflow: "main",
		IRHash: mustHashIR(t, smallIR(false))})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Journal.Close()
	if len(fresh.Costs) != 0 {
		t.Fatalf("fresh seeds carry no prior costs, got %+v", fresh.Costs)
	}
}
