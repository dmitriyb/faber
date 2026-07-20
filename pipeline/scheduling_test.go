package pipeline

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/metering"
)

// diamondIR is a -> {b, c} -> d.
func diamondIR() *config.IR {
	return testIR("w",
		[]config.Node{agentNode("w/a", "out"), agentNode("w/b", "out"), agentNode("w/c", "out"), agentNode("w/d", "out")},
		[]config.Edge{orderEdge("w/a", "w/b"), orderEdge("w/a", "w/c"), orderEdge("w/b", "w/d"), orderEdge("w/c", "w/d")},
	)
}

// Verifies 8879dc1597d6: on the diamond, b and c run concurrently
// (overlapping wall-clock intervals) and d starts only after both settle.
func TestScheduling_DiamondParallelism(t *testing.T) {
	h := newHarness(t)
	h.boxes.script("w/b", okPayload(map[string]any{"out": "b"})).latency = 60 * time.Millisecond
	h.boxes.script("w/c", okPayload(map[string]any{"out": "c"})).latency = 60 * time.Millisecond

	if err := h.run(t, diamondIR(), config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	wantStates(t, h.states(t, "run-test"), map[string]string{
		"w/a": StateOK, "w/b": StateOK, "w/c": StateOK, "w/d": StateOK,
	})

	b, c, d := h.boxes.spans["w/b"], h.boxes.spans["w/c"], h.boxes.spans["w/d"]
	if !(b[0].Before(c[1]) && c[0].Before(b[1])) {
		t.Errorf("b %v and c %v did not overlap", b, c)
	}
	if d[0].Before(b[1]) || d[0].Before(c[1]) {
		t.Errorf("d started %v before both b (%v) and c (%v) finished", d[0], b[1], c[1])
	}
}

// Verifies 8879dc1597d6: with --max-parallel 1 the diamond serializes and the
// same-level tie-break is stable name order (b before c).
func TestScheduling_MaxParallelOneSerializesInIDOrder(t *testing.T) {
	h := newHarness(t)
	h.boxes.script("w/b", okPayload(map[string]any{"out": "b"})).latency = 20 * time.Millisecond
	h.boxes.script("w/c", okPayload(map[string]any{"out": "c"})).latency = 20 * time.Millisecond

	if err := h.run(t, diamondIR(), config.RunOptions{MaxParallel: 1}); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := strings.Join(h.boxes.startOrder(), ",")
	want := "w/a,w/b,w/c,w/d"
	if got != want {
		t.Errorf("start order %q, want %q", got, want)
	}
	b, c := h.boxes.spans["w/b"], h.boxes.spans["w/c"]
	if c[0].Before(b[1]) {
		t.Errorf("c started %v before b finished %v under max-parallel 1", c[0], b[1])
	}
}

// Verifies 8879dc1597d6: the 12-way fan-out dispatched with --max-parallel 1
// yields the identical dispatch sequence across 50 runs (deterministic
// tie-break by node id).
func TestScheduling_DeterministicTieBreak(t *testing.T) {
	var nodes []config.Node
	for _, suffix := range []string{"b", "k", "a", "f", "c", "j", "d", "h", "e", "l", "g", "i"} {
		nodes = append(nodes, agentNode("w/"+suffix, "out"))
	}
	ir := testIR("w", nodes, nil)

	var first string
	for i := 0; i < 50; i++ {
		h := newHarness(t)
		if err := h.run(t, ir, config.RunOptions{MaxParallel: 1, RunID: "run-test"}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		got := strings.Join(h.boxes.startOrder(), ",")
		if i == 0 {
			first = got
			want := "w/a,w/b,w/c,w/d,w/e,w/f,w/g,w/h,w/i,w/j,w/k,w/l"
			if got != want {
				t.Fatalf("dispatch order %q, want %q", got, want)
			}
			continue
		}
		if got != first {
			t.Fatalf("run %d dispatch order %q differs from first %q", i, got, first)
		}
	}
}

// Verifies a0f44481f57b: a step failing after its declared retries marks
// every transitive dependent skipped with the root ancestor recorded, while
// an independent chain runs to completion; the run's exit is nonzero.
func TestScheduling_FailurePropagationSparesIndependentBranches(t *testing.T) {
	aNodes, aEdges := chain("w/a", 3)
	bNodes, bEdges := chain("w/b", 3)
	aNodes[0].Retry = 2
	ir := testIR("w", append(aNodes, bNodes...), append(aEdges, bEdges...))

	h := newHarness(t)
	h.boxes.script("w/a/s0", failedResult("agent", "the box died"))

	err := h.run(t, ir, config.RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("want a failed-run error, got %v", err)
	}
	states := h.states(t, "run-test")
	wantStates(t, states, map[string]string{
		"w/a/s0": StateFailed,
		"w/a/s1": StateSkippedDependency,
		"w/a/s2": StateSkippedDependency,
		"w/b/s0": StateOK,
		"w/b/s1": StateOK,
		"w/b/s2": StateOK,
	})
	if got := h.boxes.attempts("w/a/s0"); got != 3 {
		t.Errorf("failing step ran %d attempts, want 3 (retry: 2)", got)
	}
	for _, id := range []string{"w/a/s1", "w/a/s2"} {
		rec := h.record(t, "run-test", id)
		if rec.Result.Error == nil || rec.Result.Error.Detail != "w/a/s0" {
			t.Errorf("%s: skip record names ancestor %v, want w/a/s0", id, rec.Result.Error)
		}
		if rec.InputHash != "" {
			t.Errorf("%s: skip record carries input hash %q, want null", id, rec.InputHash)
		}
	}
}

// Verifies 8879dc1597d6 and a0f44481f57b: a rate-limit failure carrying a
// reset time re-enters at the injected clock's wake as a defer — consuming no
// retry attempt — and the settled record carries the defer history.
func TestScheduling_RateLimitDeferAndReadmission(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	reset := testBase.Add(90 * time.Second)

	h := newHarness(t)
	h.boxes.script("w/x",
		failedResult(metering.ReasonRateLimit, `{"reset": `+timestamp(reset)+`}`),
		okPayload(map[string]any{"out": "done"}),
	)

	if err := h.run(t, ir, config.RunOptions{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := h.boxes.attempts("w/x"); got != 2 {
		t.Fatalf("box ran %d attempts, want 2 (defer then success)", got)
	}
	rec := h.record(t, "run-test", "w/x")
	if rec.Result.Status != failure.StatusOK {
		t.Fatalf("settled %s, want ok", rec.Result.Status)
	}
	// Retry was 0: the rate-limited attempt consumed no retry budget.
	if rec.Result.Attempt != 1 {
		t.Errorf("final attempt %d, want 1 (defer consumes no retry)", rec.Result.Attempt)
	}
	deferred, wait, _, _ := decodeAnnotations(rec.Result.Attempts)
	if deferred != 1 {
		t.Errorf("defer annotations %d, want 1", deferred)
	}
	if wait != 90*time.Second {
		t.Errorf("deferred wait %s, want 90s", wait)
	}
	delays := h.clock.recorded()
	if len(delays) != 1 || delays[0] != 90*time.Second {
		t.Errorf("clock wake delays %v, want [90s]", delays)
	}
	if !strings.Contains(h.text.String(), "deferred=1") {
		t.Errorf("report does not show the defer count:\n%s", h.text.String())
	}
}

// fakeMeter scripts metering estimates; Actual reports fixed costs.
type fakeMeter struct {
	mu      sync.Mutex
	est     []metering.Estimate
	i       int
	units   []metering.Unit
	actuals []metering.Cost
}

func (m *fakeMeter) Estimate(context.Context, metering.Step) (metering.Estimate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.est) == 0 {
		return metering.Estimate{}, nil
	}
	idx := m.i
	if idx >= len(m.est) {
		idx = len(m.est) - 1
	}
	m.i++
	return m.est[idx], nil
}

func (m *fakeMeter) Actual(context.Context, metering.ResultView) ([]metering.Cost, error) {
	return m.actuals, nil
}

func (m *fakeMeter) Units() []metering.Unit { return m.units }

// schedHarness drives the scheduler directly (white box) for meter admission
// scenarios the Executor's config file path cannot script.
type schedHarness struct {
	s     *scheduler
	store *failure.Store
	seed  *failure.RunSeed
	boxes *fakeBoxes
	clock *fakeClock
}

func newSchedHarness(t *testing.T, ir *config.IR, admit admissionMeter, maxPar int) *schedHarness {
	t.Helper()
	store := failure.NewStore(t.TempDir(), nil)
	hash, err := config.HashIR(ir)
	if err != nil {
		t.Fatalf("hash IR: %v", err)
	}
	seed, err := store.Fresh(failure.Header{RunID: "run-s", Workflow: ir.Workflow, IRHash: hash, Started: testBase})
	if err != nil {
		t.Fatalf("fresh run: %v", err)
	}
	t.Cleanup(func() { seed.Journal.Close() })
	ce, err := newCondEval()
	if err != nil {
		t.Fatalf("cond env: %v", err)
	}
	g := newGraph()
	if _, err := g.addLevel(ir, &scopeEnv{params: map[string]any{}}, ce); err != nil {
		t.Fatalf("build graph: %v", err)
	}
	boxes := newFakeBoxes()
	clock := newFakeClock()
	s := &scheduler{
		g:       g,
		ce:      ce,
		scopes:  map[string]*scopeEnv{},
		maxPar:  maxPar,
		events:  make(chan event),
		done:    make(chan struct{}),
		seed:    seed,
		journal: seed.Journal,
		policy:  &failure.Policy{Journal: seed.Journal},
		admit:   admit,
		rld:     metering.NewRateLimitDefer(metering.DeferPolicy{}, nil),
		boxes:   boxes,
		images:  fakeTags{},
		expand:  &expander{},
		clock:   clock,
		log:     discardLogger(),
		runID:   "run-s",
		runDir:  store.RunDir("run-s"),
	}
	s.registerScopes(ir)
	return &schedHarness{s: s, store: store, seed: seed, boxes: boxes, clock: clock}
}

// Verifies 93d829b3f3d3 (first pass) and 8879dc1597d6: a meter scripted to
// defer twice then admit re-queues the step at each wake and settles it with
// the defer history annotated; no retry is consumed.
func TestScheduling_MeterDeferThenAdmit(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	until := testBase.Add(time.Minute)
	meter := &fakeMeter{est: []metering.Estimate{
		{DeferUntil: &until},
		{DeferUntil: &until},
		{},
	}}
	admit := metering.NewAdmitter(map[string][]metering.Meter{"": {meter}}, nil, nil)
	sh := newSchedHarness(t, ir, admit, 0)

	if err := sh.s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	rec := rp.LastByStep["w/x"]
	if rec.Result.Status != failure.StatusOK {
		t.Fatalf("settled %s, want ok", rec.Result.Status)
	}
	deferred, _, _, _ := decodeAnnotations(rec.Result.Attempts)
	if deferred != 2 {
		t.Errorf("defer annotations %d, want 2", deferred)
	}
	if got := sh.boxes.attempts("w/x"); got != 1 {
		t.Errorf("box ran %d attempts, want 1", got)
	}
	if len(sh.clock.recorded()) != 2 {
		t.Errorf("clock wakes %d, want 2", len(sh.clock.recorded()))
	}
}

// Verifies a0f44481f57b: a meter rejection settles the node failed with the
// structured budget error, and propagation is indistinguishable from any
// other failure.
func TestScheduling_MeterRejectFailsAndPropagates(t *testing.T) {
	ir := testIR("w",
		[]config.Node{agentNode("w/x", "out"), agentNode("w/y", "out")},
		[]config.Edge{orderEdge("w/x", "w/y")},
	)
	meter := &fakeMeter{
		est:   []metering.Estimate{{Costs: []metering.Cost{{Unit: "tokens", Amount: 100}}}},
		units: []metering.Unit{"tokens"},
	}
	admit := metering.NewAdmitter(map[string][]metering.Meter{"": {meter}}, map[metering.Unit]int64{"tokens": 10}, nil)
	sh := newSchedHarness(t, ir, admit, 0)

	if err := sh.s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	x := rp.LastByStep["w/x"]
	if recordState(x) != StateFailed || x.Result.Error.Reason != reasonBudget {
		t.Fatalf("x settled %s/%v, want failed with budget reason", recordState(x), x.Result.Error)
	}
	if !strings.Contains(x.Result.Error.Detail, "tokens") {
		t.Errorf("budget error does not name the unit: %s", x.Result.Error.Detail)
	}
	if y := rp.LastByStep["w/y"]; recordState(y) != StateSkippedDependency {
		t.Errorf("y settled %s, want skipped-dependency", recordState(y))
	}
	if sh.boxes.attempts("w/x") != 0 {
		t.Errorf("rejected step still ran a box")
	}
}

// Verifies 8879dc1597d6 and 990c3d8a7888: a journal that refuses appends
// aborts the run process-level, and the partial journal still renders a
// report with absent lines for undispatched nodes.
func TestScheduling_JournalAppendFailureAbortsRun(t *testing.T) {
	ir := testIR("w",
		[]config.Node{agentNode("w/a", "out"), agentNode("w/b", "out")},
		[]config.Edge{orderEdge("w/a", "w/b")},
	)
	admit := metering.NewAdmitter(nil, nil, nil)
	sh := newSchedHarness(t, ir, admit, 0)
	sh.seed.Journal.Close() // durability gone before the first settle

	err := sh.s.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "journal") {
		t.Fatalf("want a journal-abort error, got %v", err)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatalf("load partial journal: %v", err)
	}
	report, err := (RunReporter{}).Report(rp, ir, "journal.jsonl")
	if err != nil {
		t.Fatalf("report over partial journal: %v", err)
	}
	if report.Run.Totals.Absent == 0 {
		t.Errorf("partial-run report shows no absent lines: %+v", report.Run.Totals)
	}
	for _, line := range report.Steps {
		if line.Status != StateAbsent {
			t.Errorf("step %s reported %s, want absent (nothing was journaled)", line.ID, line.Status)
		}
	}
}

// Verifies 8879dc1597d6: budget contention defers with a zero wake time
// re-check on the next settlement rather than at a wall-clock time.
func TestScheduling_ZeroUntilDeferWakesOnSettlement(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/a", "out"), agentNode("w/b", "out")}, nil)
	meter := &fakeMeter{
		est:     []metering.Estimate{{Costs: []metering.Cost{{Unit: "tokens", Amount: 60}}}},
		units:   []metering.Unit{"tokens"},
		actuals: []metering.Cost{{Unit: "tokens", Amount: 10}},
	}
	admit := metering.NewAdmitter(map[string][]metering.Meter{"": {meter}}, map[metering.Unit]int64{"tokens": 100}, nil)
	sh := newSchedHarness(t, ir, admit, 0)
	sh.boxes.script("w/a", okPayload(map[string]any{"out": "a"})).latency = 40 * time.Millisecond
	sh.boxes.script("w/b", okPayload(map[string]any{"out": "b"})).latency = 40 * time.Millisecond

	if err := sh.s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	deferredTotal := 0
	for _, id := range []string{"w/a", "w/b"} {
		rec := rp.LastByStep[id]
		if rec.Result.Status != failure.StatusOK {
			t.Fatalf("%s settled %s, want ok", id, rec.Result.Status)
		}
		d, _, _, _ := decodeAnnotations(rec.Result.Attempts)
		deferredTotal += d
	}
	// Both estimates cannot fit alongside each other's reservation; whichever
	// admitted second deferred until the first settled.
	if deferredTotal != 1 {
		t.Errorf("total defers %d, want exactly 1 (contention defer)", deferredTotal)
	}
	if len(sh.clock.recorded()) != 0 {
		t.Errorf("zero-until defer armed a wall-clock wake: %v", sh.clock.recorded())
	}
}

func timestamp(ts time.Time) string { return strconv.FormatInt(ts.Unix(), 10) }

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// raceAdmitter forces the adverse zero-until ordering: the contended node's
// Admit blocks until the holder's settlement has been FOLDED (its record is
// in the journal), then defers with a zero wake — so the evDeferred arrives
// after the settlement that would have drained the parking lot.
type raceAdmitter struct {
	store    *failure.Store
	runID    string
	holder   string
	contende string
	mu       sync.Mutex
	deferred int
}

func (f *raceAdmitter) Admit(ctx context.Context, s metering.Step) (metering.Decision, error) {
	if s.NodeID != f.contende {
		return metering.Decision{Kind: metering.Admit}, nil
	}
	f.mu.Lock()
	already := f.deferred
	f.mu.Unlock()
	if already > 0 {
		return metering.Decision{Kind: metering.Admit}, nil
	}
	// Wait until the holder's ok record reaches the journal — the append
	// happens on the loop goroutine while folding its evSettled, so by the
	// time this defer decision travels back, that settlement is history.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rp, err := f.store.Load(f.runID); err == nil {
			if rec, ok := rp.LastByStep[f.holder]; ok && rec.Result.Status == failure.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			return metering.Decision{}, context.DeadlineExceeded
		}
		time.Sleep(2 * time.Millisecond)
	}
	f.mu.Lock()
	f.deferred++
	f.mu.Unlock()
	return metering.Decision{Kind: metering.Defer, Detail: "contended"}, nil // zero Until
}

func (f *raceAdmitter) Settle(context.Context, metering.ResultView) ([]metering.Cost, error) {
	return nil, nil
}

// Verifies 8879dc1597d6: a zero-until defer whose triggering settlement was
// folded before the defer event arrived does not park forever — the scheduler
// re-checks the parking lot when nothing is left in flight, and the node
// re-admits and settles.
func TestScheduling_ZeroUntilDeferAfterHolderSettled(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/a", "out"), agentNode("w/b", "out")}, nil)
	store := failure.NewStore(t.TempDir(), nil)
	admit := &raceAdmitter{store: store, runID: "run-s", holder: "w/a", contende: "w/b"}

	hash, err := config.HashIR(ir)
	if err != nil {
		t.Fatalf("hash IR: %v", err)
	}
	seed, err := store.Fresh(failure.Header{RunID: "run-s", Workflow: ir.Workflow, IRHash: hash, Started: testBase})
	if err != nil {
		t.Fatalf("fresh run: %v", err)
	}
	t.Cleanup(func() { seed.Journal.Close() })
	ce, err := newCondEval()
	if err != nil {
		t.Fatalf("cond env: %v", err)
	}
	g := newGraph()
	if _, err := g.addLevel(ir, &scopeEnv{params: map[string]any{}}, ce); err != nil {
		t.Fatalf("build graph: %v", err)
	}
	boxes := newFakeBoxes()
	s := &scheduler{
		g:       g,
		ce:      ce,
		scopes:  map[string]*scopeEnv{},
		events:  make(chan event),
		done:    make(chan struct{}),
		seed:    seed,
		journal: seed.Journal,
		policy:  &failure.Policy{Journal: seed.Journal},
		admit:   admit,
		rld:     metering.NewRateLimitDefer(metering.DeferPolicy{}, nil),
		boxes:   boxes,
		images:  fakeTags{},
		expand:  &expander{},
		clock:   newFakeClock(),
		log:     discardLogger(),
		runID:   "run-s",
		runDir:  store.RunDir("run-s"),
	}
	s.registerScopes(ir)

	doneCh := make(chan error, 1)
	go func() { doneCh <- s.run(context.Background()) }()
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("scheduler hung: zero-until defer parked with nothing in flight")
	}
	rp, err := store.Load("run-s")
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	for _, id := range []string{"w/a", "w/b"} {
		if rec := rp.LastByStep[id]; rec.Result.Status != failure.StatusOK {
			t.Errorf("%s settled %s, want ok", id, rec.Result.Status)
		}
	}
	d, _, _, _ := decodeAnnotations(rp.LastByStep["w/b"].Result.Attempts)
	if d != 1 {
		t.Errorf("contended node carries %d defer annotations, want 1", d)
	}
}

// costingAdmitter admits everything and reports a fixed cost per settled
// attempt.
type costingAdmitter struct{}

func (costingAdmitter) Admit(context.Context, metering.Step) (metering.Decision, error) {
	return metering.Decision{Kind: metering.Admit}, nil
}

func (costingAdmitter) Settle(context.Context, metering.ResultView) ([]metering.Cost, error) {
	return []metering.Cost{{Unit: "u", Amount: 5}}, nil
}

// Verifies a0f44481f57b and 990c3d8a7888: attempt costs settled before a
// rate-limit defer conversion are not dropped — they merge into the eventual
// settlement's cost record alongside the re-admitted attempt's costs.
func TestScheduling_DeferConversionCostsJournaled(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	sh := newSchedHarness(t, ir, costingAdmitter{}, 0)
	reset := testBase.Add(30 * time.Second)
	sh.boxes.script("w/x",
		failedResult(metering.ReasonRateLimit, `{"reset": `+timestamp(reset)+`}`),
		okPayload(map[string]any{"out": "done"}),
	)

	if err := sh.s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatalf("load journal: %v", err)
	}
	if rec := rp.LastByStep["w/x"]; rec.Result.Status != failure.StatusOK {
		t.Fatalf("settled %s, want ok", rec.Result.Status)
	}
	var costs []metering.Cost
	for _, c := range rp.Costs {
		if c.StepID != "w/x" {
			continue
		}
		var batch []metering.Cost
		if err := json.Unmarshal(c.Cost, &batch); err != nil {
			t.Fatalf("decode cost record: %v", err)
		}
		costs = append(costs, batch...)
	}
	if len(costs) != 2 {
		t.Fatalf("journaled %d attempt costs, want 2 (deferred attempt + settled attempt): %v", len(costs), costs)
	}
	for _, c := range costs {
		if c.Unit != "u" || c.Amount != 5 {
			t.Errorf("cost %+v, want {u 5}", c)
		}
	}
}

// Verifies a0f44481f57b: restoreDeferCounts only restores from failed records
// (a step that later settled ok reset its counter), counts only the trailing
// consecutive scheduler-authored defer annotations, and never trusts a
// box-authored "deferred" failure reason in the real attempt history.
func TestExecutor_RestoreDeferCounts(t *testing.T) {
	annot := func() failure.AttemptInfo {
		return failure.AttemptInfo{ // Attempt == 0: the scheduler's marker
			Timing: failure.Timing{Started: testBase, Finished: testBase.Add(time.Minute)},
			Error:  &failure.ErrorRecord{Reason: reasonDeferred, Detail: "rate-limited"},
		}
	}
	realAttempt := func(reason string) failure.AttemptInfo {
		return failure.AttemptInfo{
			Attempt: 1,
			Timing:  failure.Timing{Started: testBase, Finished: testBase.Add(time.Second)},
			Error:   &failure.ErrorRecord{Reason: reason},
		}
	}
	seed := &failure.RunSeed{Prior: map[failure.Key]failure.ResultRecord{
		// Settled ok after defers: counter was reset, nothing restores.
		{StepID: "w/ok", InputHash: "h1"}: {StepID: "w/ok", Result: failure.Result{
			Status: failure.StatusOK, Payload: json.RawMessage(`{}`), Attempt: 1,
			Attempts: []failure.AttemptInfo{annot(), annot(), annot()},
		}},
		// Failed with two trailing defers behind a real failed attempt.
		{StepID: "w/fail", InputHash: "h2"}: {StepID: "w/fail", Result: failure.Result{
			Status: failure.StatusFailed, Error: &failure.ErrorRecord{Reason: "agent"}, Attempt: 1,
			Attempts: []failure.AttemptInfo{annot(), realAttempt("agent"), annot(), annot()},
		}},
		// Box-authored "deferred" reasons in real history are not annotations.
		{StepID: "w/hostile", InputHash: "h3"}: {StepID: "w/hostile", Result: failure.Result{
			Status: failure.StatusFailed, Error: &failure.ErrorRecord{Reason: reasonDeferred}, Attempt: 2,
			Attempts: []failure.AttemptInfo{realAttempt(reasonDeferred), realAttempt(reasonDeferred)},
		}},
	}}
	rld := metering.NewRateLimitDefer(metering.DeferPolicy{}, nil)
	restoreDeferCounts(rld, seed)
	if got := rld.Consecutive("w/ok"); got != 0 {
		t.Errorf("ok record restored %d consecutive defers, want 0", got)
	}
	if got := rld.Consecutive("w/fail"); got != 2 {
		t.Errorf("failed record restored %d consecutive defers, want 2 (trailing only)", got)
	}
	if got := rld.Consecutive("w/hostile"); got != 0 {
		t.Errorf("box-authored defer reasons restored %d, want 0", got)
	}
}

// scriptedAdmit answers Admit per node id from a queue of decisions (the last
// repeats); Settle records views. It fakes the admitter to force adverse
// admission orderings the real ledger cannot script deterministically.
type scriptedAdmit struct {
	mu      sync.Mutex
	decs    map[string][]metering.Decision
	settled []metering.ResultView
}

func (a *scriptedAdmit) Admit(_ context.Context, s metering.Step) (metering.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	q := a.decs[s.NodeID]
	if len(q) == 0 {
		return metering.Decision{Kind: metering.Admit}, nil
	}
	d := q[0]
	if len(q) > 1 {
		a.decs[s.NodeID] = q[1:]
	}
	return d, nil
}

func (a *scriptedAdmit) Settle(_ context.Context, r metering.ResultView) ([]metering.Cost, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.settled = append(a.settled, r)
	return []metering.Cost{{Unit: "tokens", Amount: 1}}, nil
}

// manualClock is a Clock whose AfterFunc callbacks fire only when the test
// releases them — the stall window between a timed defer and its wake stays
// open as long as the test needs.
type manualClock struct {
	mu  sync.Mutex
	cbs []func()
}

func (c *manualClock) Now() time.Time { return testBase }
func (c *manualClock) AfterFunc(_ time.Duration, f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cbs = append(c.cbs, f)
}
func (c *manualClock) fire() {
	c.mu.Lock()
	cbs := c.cbs
	c.cbs = nil
	c.mu.Unlock()
	for _, f := range cbs {
		go f()
	}
}

// Verifies 8879dc1597d6 (CC-F2): a timed defer wakes zero-until parked nodes.
// Node a parks awaiting the next settlement; node b's timed defer is not a
// settlement — without the wake, a would stall for b's whole rate-limit
// window with admissible work queued.
func TestScheduling_TimedDeferWakesParked(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/a", "out"), agentNode("w/b", "out")}, nil)
	admit := &scriptedAdmit{decs: map[string][]metering.Decision{
		"w/a": {{Kind: metering.Defer}, {Kind: metering.Admit}},                                 // zero-until park, then admit
		"w/b": {{Kind: metering.Defer, Until: testBase.Add(time.Hour)}, {Kind: metering.Admit}}, // timed defer
	}}
	sh := newSchedHarness(t, ir, admit, 0)
	clock := &manualClock{}
	sh.s.clock = clock

	done := make(chan error, 1)
	go func() { done <- sh.s.run(context.Background()) }()

	// a must settle on b's timed defer alone — no timer has fired.
	deadline := time.After(5 * time.Second)
	for {
		rp, err := sh.store.Load("run-s")
		if err == nil {
			if rec, ok := rp.LastByStep["w/a"]; ok && rec.Result.Status == failure.StatusOK {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatal("parked node a never woke on the timed defer (stalled for the rate-limit window)")
		case <-time.After(5 * time.Millisecond):
		}
	}
	clock.fire() // release b's wake so the run can finish
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

// Verifies 8879dc1597d6 / 58879b841ed4 (L-P1b): every retry attempt passes
// admission and settles its own costs — attempt 2 is metered, not a free
// ride outside the ledger.
func TestScheduling_RetryAttemptsAdmittedAndSettled(t *testing.T) {
	ir := testIR("w", []config.Node{{ID: "w/x", Kind: config.KindAgent,
		Template: testTemplate("tpl-x", "out"), Retry: 1, Bindings: map[string]config.BindingDesc{}}}, nil)
	admit := &scriptedAdmit{decs: map[string][]metering.Decision{}}
	sh := newSchedHarness(t, ir, admit, 0)
	sh.boxes.script("w/x", failedResult("agent", "died"), okPayload(map[string]any{"out": "v"}))

	if err := sh.s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sh.boxes.attempts("w/x"); got != 2 {
		t.Fatalf("box ran %d attempts, want 2", got)
	}
	if len(admit.settled) != 2 {
		t.Fatalf("Settle ran %d times, want once per attempt (2)", len(admit.settled))
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Costs) != 1 {
		t.Fatalf("want one cost record, got %d", len(rp.Costs))
	}
	if got := string(rp.Costs[0].Cost); !strings.Contains(got, `"amount":1`) || strings.Count(got, `"tokens"`) != 2 {
		t.Fatalf("cost record must carry both attempts' actuals, got %s", got)
	}
}

// Verifies 58879b841ed4 (L-P1b): a budget rejection at a retry's re-admission
// is terminal — the step settles failed with reason budget and the real
// attempt's history, without burning the remaining retries on refusals.
func TestScheduling_RetryBudgetRejectIsTerminal(t *testing.T) {
	ir := testIR("w", []config.Node{{ID: "w/x", Kind: config.KindAgent,
		Template: testTemplate("tpl-x", "out"), Retry: 3, Bindings: map[string]config.BindingDesc{}}}, nil)
	admit := &scriptedAdmit{decs: map[string][]metering.Decision{
		"w/x": {{Kind: metering.Admit}, {Kind: metering.Reject, Budget: "tokens", Detail: "budget tokens exhausted"}},
	}}
	sh := newSchedHarness(t, ir, admit, 0)
	sh.boxes.script("w/x", failedResult("agent", "died"))

	if err := sh.s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sh.boxes.attempts("w/x"); got != 1 {
		t.Fatalf("box ran %d attempts, want 1 (the reject must not burn retries)", got)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatal(err)
	}
	rec := rp.LastByStep["w/x"]
	if rec.Result.Status != failure.StatusFailed || rec.Result.Error.Reason != reasonBudget {
		t.Fatalf("want a terminal budget failure, got %+v", rec.Result)
	}
	if len(rec.Result.Attempts) != 1 || rec.Result.Attempts[0].Error.Reason != "agent" {
		t.Fatalf("the real attempt's history must ride along, got %+v", rec.Result.Attempts)
	}
}

// Verifies 8879dc1597d6 / flow_admission's ordering invariant (L-P3f): every
// defer is journaled when it happens — a crash mid-wait leaves the waiting
// timeline on disk, not only in the eventually-settled record's annotations.
func TestScheduling_DeferJournaledDurably(t *testing.T) {
	ir := testIR("w", []config.Node{agentNode("w/x", "out")}, nil)
	admit := &scriptedAdmit{decs: map[string][]metering.Decision{
		"w/x": {{Kind: metering.Defer, Until: testBase.Add(time.Minute), Detail: "endpoint saturated"}, {Kind: metering.Admit}},
	}}
	sh := newSchedHarness(t, ir, admit, 0)
	if err := sh.s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Defers) != 1 || rp.Defers[0].StepID != "w/x" || rp.Defers[0].Detail != "endpoint saturated" {
		t.Fatalf("defer event not journaled durably: %+v", rp.Defers)
	}
	if !rp.Defers[0].Until.Equal(testBase.Add(time.Minute)) {
		t.Fatalf("defer until lost: %+v", rp.Defers[0])
	}
}

// Verifies 58879b841ed4 (F3-review): costs settled by a defer-converted
// dispatch survive even when the node's eventual settle takes a cheap-gate
// path (not evSettled) — the stash is folded at the single settle point, so
// no settled actuals are dropped. Here the node rate-limit-defers (settling a
// cost), then its wake hits a budget reject that settles via the reshaped
// failure — the stashed cost must still reach the journal.
func TestScheduling_DeferStashedCostSurvivesReject(t *testing.T) {
	ir := testIR("w", []config.Node{{ID: "w/x", Kind: config.KindAgent,
		Template: testTemplate("tpl-x", "out"), Retry: 2, Bindings: map[string]config.BindingDesc{}}}, nil)
	// Meter: every attempt actuals 3 tokens; budget 100 (so the first attempt
	// admits and settles a cost, then the rate-limit defer re-queues).
	meter := &fakeMeter{units: []metering.Unit{"tokens"}, actuals: []metering.Cost{{Unit: "tokens", Amount: 3}}}
	admit := metering.NewAdmitter(map[string][]metering.Meter{"": {meter}}, map[metering.Unit]int64{"tokens": 100}, nil)
	sh := newSchedHarness(t, ir, admit, 0)
	// First attempt rate-limits (defer, cost stashed); the re-dispatch succeeds.
	rl := failedResult(metering.ReasonRateLimit, `{"reset":0}`)
	sh.boxes.script("w/x", rl, okPayload(map[string]any{"out": "v"}))

	if err := sh.s.run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	rp, err := sh.store.Load("run-s")
	if err != nil {
		t.Fatal(err)
	}
	// The stashed rate-limited attempt's cost must be journaled, not dropped.
	var total int64
	for _, c := range rp.Costs {
		var costs []metering.Cost
		if err := json.Unmarshal(c.Cost, &costs); err != nil {
			t.Fatal(err)
		}
		for _, cost := range costs {
			total += cost.Amount
		}
	}
	if total < 3 {
		t.Fatalf("stashed defer cost dropped: journaled %d tokens, want >= 3 (the rate-limited attempt's actual)", total)
	}
}
