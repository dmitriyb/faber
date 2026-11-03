package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/pipeline"
)

const refConfig = "../config/testdata/reference.yaml"

var (
	t0    = time.Date(2026, 9, 2, 19, 4, 8, 0, time.UTC)
	fixed = t0.Add(45 * time.Minute)
)

func okRes(start, finish time.Time, payload string) failure.Result {
	return failure.Result{Status: failure.StatusOK, Payload: json.RawMessage(payload), Attempt: 1,
		Timing: failure.Timing{Started: start, Finished: finish}}
}

func failRes(start, finish time.Time) failure.Result {
	return failure.Result{Status: failure.StatusFailed, Attempt: 1,
		Error:  &failure.ErrorRecord{Reason: "agent-failed", Detail: "agent exited 1", Handoff: "boxes/x/attempt-1/result/handoff.json"},
		Timing: failure.Timing{Started: start, Finished: finish}}
}

func skipRes(reason, ancestor string, at time.Time) failure.Result {
	return failure.Result{Status: failure.StatusFailed, Attempt: 1, Error: &failure.ErrorRecord{Reason: reason, Detail: ancestor},
		Timing: failure.Timing{Started: at, Finished: at}}
}

// seedStore writes two task-workflow runs against the reference config: a
// failed one (review failed at iteration 2, merge not reached) and a live
// one (implement ok, review started and still running). The live run's
// lock is held by the test for its duration.
func seedStore(t *testing.T) (*failure.Store, func()) {
	t.Helper()
	root := t.TempDir()
	store := failure.NewStore(root, nil)
	abs, _ := filepath.Abs(refConfig)
	ir, err := config.Desugar(mustLoadRef(t), "task")
	if err != nil {
		t.Fatal(err)
	}
	irHash, _ := config.HashIR(ir)
	hdr := func(id string, started time.Time) failure.Header {
		return failure.Header{RunID: id, ConfigPath: abs, Workflow: "task", Params: map[string]string{"repo": "sandbox", "item": "I-1"},
			IRHash: irHash, IRVersion: ir.IRVersion, Started: started}
	}
	// failed run
	j, err := store.Begin(hdr("20260902-170408-d0feb66b", t0))
	if err != nil {
		t.Fatal(err)
	}
	add := func(j *failure.Journal, id string, attempt int, s time.Time, res failure.Result, executed bool) {
		t.Helper()
		if executed {
			if err := j.AppendStart(failure.StartRecord{StepID: id, Attempt: attempt, Started: s}); err != nil {
				t.Fatal(err)
			}
		}
		hash := ""
		if executed {
			hash = "h-" + id
		}
		if err := j.AppendResult(failure.ResultRecord{StepID: id, InputHash: hash, Result: res}); err != nil {
			t.Fatal(err)
		}
	}
	add(j, "task/implement", 1, t0, okRes(t0, t0.Add(3*time.Minute), `{"branch":"work/x","pr":17}`), true)
	add(j, "task/review-cycle@1/review", 1, t0.Add(3*time.Minute), okRes(t0.Add(3*time.Minute), t0.Add(7*time.Minute), `{"verdict":"changes"}`), true)
	add(j, "task/review-cycle@1/fix", 1, t0.Add(7*time.Minute), okRes(t0.Add(7*time.Minute), t0.Add(12*time.Minute), `{"status":"pushed"}`), true)
	add(j, "task/review-cycle@2/review", 1, t0.Add(12*time.Minute), failRes(t0.Add(12*time.Minute), t0.Add(15*time.Minute)), true)
	add(j, "task/review-cycle@2/fix", 0, t0.Add(15*time.Minute), skipRes("skipped-dependency", "task/review-cycle@2/review", t0.Add(15*time.Minute)), false)
	add(j, "task/review-cycle@3/review", 0, t0.Add(15*time.Minute), skipRes("skipped-dependency", "task/review-cycle@2/review", t0.Add(15*time.Minute)), false)
	add(j, "task/review-cycle@3/fix", 0, t0.Add(15*time.Minute), skipRes("skipped-dependency", "task/review-cycle@2/review", t0.Add(15*time.Minute)), false)
	add(j, "task/review", 0, t0.Add(15*time.Minute), skipRes("skipped-dependency", "task/review-cycle@2/review", t0.Add(15*time.Minute)), false)
	add(j, "task/merge", 0, t0.Add(15*time.Minute), skipRes("skipped-dependency", "task/review-cycle@2/review", t0.Add(15*time.Minute)), false)
	if err := j.AppendRunEnd(failure.RunEndRecord{Status: failure.RunEndSettled, Failed: 1, Finished: t0.Add(15 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	// an output log for the failed review
	dir := pipeline.AttemptDir(store.RunDir("20260902-170408-d0feb66b"), "task/review-cycle@2/review", 1)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, pipeline.OutputLogName), []byte("prelude: fetch-pr #17 ok\nagent: claude --print ... started\nagent: exit 1\n"), 0o644)

	// live run
	live := "20260915-180538-4fc8c4f3"
	j2, err := store.Begin(hdr(live, fixed.Add(-20*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	ls := fixed.Add(-20 * time.Minute)
	add(j2, "task/implement", 1, ls, okRes(ls, ls.Add(16*time.Minute), `{"branch":"work/y","pr":18}`), true)
	if err := j2.AppendStart(failure.StartRecord{StepID: "task/review-cycle@1/review", Attempt: 1, Started: fixed.Add(-4 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	j2.Close()
	lock, err := failure.AcquireRunLock(store.RunDir(live))
	if err != nil {
		t.Fatal(err)
	}
	return store, func() { lock.Release() }
}

func mustLoadRef(t *testing.T) *config.Config {
	t.Helper()
	cfg, viols, err := config.Load(refConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.Validate(cfg, viols); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testModel(t *testing.T, store *failure.Store) *model {
	t.Helper()
	abs, _ := filepath.Abs(refConfig)
	m := newApp(Options{Store: store, Now: func() time.Time { return fixed }}, config.UIOptions{ConfigPath: abs, Color: false})
	m.refresh(context.Background())
	return m
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (UPDATE_GOLDEN=1 to create): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// Verifies 1d5d7c4db2fe and 38b440a94d4c: Home and Run render as the
// designed screens at the operator's size and at 80x24, colour-stripped
// frames pinned as goldens; the live run shows its running step and the
// failed run its failure, handoff, and output tail.
func TestGoldenScreens(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	for _, size := range [][2]int{{221, 51}, {80, 24}} {
		name := func(s string) string { return s + "-" + itoa(size[0]) + "x" + itoa(size[1]) + ".txt" }
		m.screen = "home"
		m.runSel = 0
		checkGolden(t, name("home"), m.render(size[0], size[1]).plain())
		m.openRun("20260902-170408-d0feb66b")
		m.follow = true
		m.rebuildRows()
		for _, view := range []string{"tree", "graph", "timeline"} {
			m.view = view
			checkGolden(t, name("run-"+view), m.render(size[0], size[1]).plain())
		}
		m.openRun("20260915-180538-4fc8c4f3")
		m.view = "tree"
		checkGolden(t, name("run-live"), m.render(size[0], size[1]).plain())
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// Verifies f392ecd71cd1: the live view is derived from the journal alone —
// a start without a later result reads as running since its start, the
// run's badge comes from the lock, and the selected run's now line names
// the step with its elapsed time and the store-wide median.
func TestLiveDerivation(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	if len(m.runs) != 2 || m.runs[0].ID != "20260915-180538-4fc8c4f3" || m.runs[0].State != "LIVE" {
		t.Fatalf("runs = %+v, want the live run first", m.runs)
	}
	if m.runs[1].State != "FAILED" {
		t.Errorf("failed run state %q", m.runs[1].State)
	}
	m.openRun("20260915-180538-4fc8c4f3")
	if len(m.run.Running) != 1 || m.run.Running[0].Step != "task/review-cycle@1/review" {
		t.Fatalf("running = %+v", m.run.Running)
	}
	strip := stripSGR(m.nowStrip(m.st))
	if !strings.Contains(strip, "review-cycle 1/3 > review") || !strings.Contains(strip, "running  4m00s") {
		t.Errorf("now strip %q", strip)
	}
	if !strings.Contains(strip, "usually 4m00s") {
		t.Errorf("now strip lacks the median: %q", strip)
	}
	release()
	m.refresh(context.Background())
	if m.runs[0].State == "LIVE" {
		t.Error("after the lock is released the run must not read LIVE")
	}
}

// Verifies 1d5d7c4db2fe: the tree folds finished iterations to one line
// with their composition and opens the failed one; n jumps to the problem;
// enter toggles an iteration; the filter narrows rows.
func TestTreeFoldingAndKeys(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	var lines []string
	for _, r := range m.rows {
		lines = append(lines, strings.Repeat("  ", r.indent)+r.name+" | "+r.status+" | "+r.note)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"implement | ok |",
		"review-cycle | FAIL | 2 of up to 3",
		"  @1 | ok | review . fix",
		"  @2 | FAIL |",
		"    review | FAIL | agent-failed",
		"    fix | not reached | after review",
		"@3 | not reached | after review-cycle@2/review",
		"merge | not reached | after review-cycle",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rows lack %q:\n%s", want, got)
		}
	}
	m.sel = 0
	m.handle(context.Background(), key{r: 'n'})
	if r := m.selectedRow(); r == nil || r.id != "task/review-cycle@2/review" {
		t.Errorf("n selected %+v, want the failed review", r)
	}
	m.handle(context.Background(), key{r: 'k'})
	m.handle(context.Background(), key{r: 'k'})
	if r := m.selectedRow(); r == nil || r.kind != "iter" || r.id != "task/review-cycle@1/" {
		t.Fatalf("selection %+v, want iteration 1", r)
	}
	before := len(m.rows)
	m.handle(context.Background(), key{name: "enter"})
	if len(m.rows) != before+2 {
		t.Errorf("opening iteration 1 added %d rows, want 2", len(m.rows)-before)
	}
	m.handle(context.Background(), key{r: '/'})
	for _, c := range "fix" {
		m.handle(context.Background(), key{r: c})
	}
	m.handle(context.Background(), key{name: "enter"})
	for _, r := range m.rows {
		if !strings.Contains(r.id, "fix") && !strings.Contains(r.name, "fix") {
			t.Errorf("filter left %q", r.id)
		}
	}
	m.handle(context.Background(), key{r: 'q'})
	if m.screen != "home" {
		t.Error("q must return to home")
	}
}

// Verifies a22951c98fe3: quitting a run with a failed step selected prints
// the exact re-enter command; a pause on a non-live run is refused with a
// message rather than writing a marker.
func TestHonestActions(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	m.follow = true
	m.rebuildRows()
	m.handle(context.Background(), key{r: 'q'})
	if want := "faber resume 20260902-170408-d0feb66b --interactive task/review-cycle@2/review"; m.quitCmd != want {
		t.Errorf("quit command %q, want %q", m.quitCmd, want)
	}
	m.runSel = 1 // the failed run
	m.handle(context.Background(), key{r: 'p'})
	if !strings.Contains(m.status, "not live") {
		t.Errorf("pause on a settled run: status %q", m.status)
	}
	if _, err := os.Stat(filepath.Join(store.RunDir("20260902-170408-d0feb66b"), "pause")); err == nil {
		t.Error("a refused pause must not write the marker")
	}
}

// Verifies 38b440a94d4c: colour touches the role table only — status
// words, identifiers, chrome, tertiary — from the 16 palette slots, and a
// colour-stripped frame is identical to the plain render.
func TestColourIsAdditive(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	plain := m.render(120, 40).plain()
	m.st = style{color: true}
	colored := m.render(120, 40)
	if colored.plain() != plain {
		t.Error("colour changed the text")
	}
	raw := strings.Join(colored.rows, "\n")
	if !strings.Contains(raw, sgrRed+"FAIL") || !strings.Contains(raw, sgrChrome+"STEPS") || !strings.Contains(raw, sgrIdent) {
		t.Error("expected role colours missing")
	}
	if strings.Contains(raw, "\x1b[38;") || strings.Contains(raw, "\x1b[48;") {
		t.Error("256-colour sequences are not allowed")
	}
}

// Verifies 38b440a94d4c: the key reader decodes CSI arrows, enter, a lone
// escape, and plain runes.
func TestReadKeys(t *testing.T) {
	in := strings.NewReader("j\x1b[A\r\x1b[6~q")
	out := make(chan key, 8)
	readKeys(in, out)
	var got []string
	for k := range out {
		if k.name != "" {
			got = append(got, k.name)
		} else {
			got = append(got, string(k.r))
		}
	}
	if want := "j up enter pgdn q"; strings.Join(got, " ") != want {
		t.Errorf("keys %q, want %q", strings.Join(got, " "), want)
	}
}

// Verifies 1d5d7c4db2fe: the graph layout puts dependents one column
// right of their dependencies and the collapsed graph has one node per
// loop.
func TestGraphLayout(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	nodes, edges := collapsedGraph(m.run, m.rows)
	layout(nodes, edges)
	col := map[string]int{}
	for _, n := range nodes {
		col[n.id] = n.col
	}
	if col["task/implement"] != 0 || col["task/review-cycle"] != 1 || col["task/merge"] != 2 {
		t.Errorf("columns %v, want implement 0, review-cycle 1, merge 2", col)
	}
	for _, n := range nodes {
		if strings.Contains(n.id, "@") {
			t.Errorf("iteration node %s leaked into the collapsed graph", n.id)
		}
	}
}

// Verifies 1d5d7c4db2fe: the workflow preview renders the compute graph
// and the folded structure from the IR alone — loops as "up to N
// iterations" with their body once, no status words — and tab moves the
// Home focus between the panes.
func TestWorkflowPreview(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.handle(context.Background(), key{r: '\t'})
	if m.focus != "workflows" {
		t.Fatalf("tab did not switch focus: %q", m.focus)
	}
	m.handle(context.Background(), key{r: 'j'}) // epic, task sorted: task is second
	m.handle(context.Background(), key{name: "enter"})
	if m.screen != "run" || !m.preview || m.run.ID != "task" {
		t.Fatalf("enter did not open the task preview: screen %q preview %v run %v", m.screen, m.preview, m.run)
	}
	checkGolden(t, "workflow-graph-221x51.txt", m.render(221, 51).plain())
	m.view = "tree"
	checkGolden(t, "workflow-tree-221x51.txt", m.render(221, 51).plain())
	var notes []string
	for _, r := range m.rows {
		notes = append(notes, r.name+"|"+r.status+"|"+r.note)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "review-cycle||up to 3 iterations") || !strings.Contains(joined, "review||") || !strings.Contains(joined, "fix||") {
		t.Errorf("preview rows %s", joined)
	}
	for _, r := range m.rows {
		if r.status != "" && r.status != "absent" {
			t.Errorf("preview row %s carries state %q", r.id, r.status)
		}
	}
	m.handle(context.Background(), key{r: 'q'})
	if m.screen != "home" || m.preview {
		t.Error("q must return home and leave preview mode")
	}
}

// Verifies 1d5d7c4db2fe: a missing image names the tag hash it looked for.
func TestHomeShowsMissingTagHash(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	abs, _ := filepath.Abs(refConfig)
	m := newApp(Options{Store: store, Now: func() time.Time { return fixed }, Images: fakeProbe{}}, config.UIOptions{ConfigPath: abs})
	m.refresh(context.Background())
	frame := m.render(221, 51).plain()
	if !strings.Contains(frame, "MISSING  faber build  20be39bb9bb7") {
		t.Errorf("missing image line lacks the tag hash:\n%s", frame)
	}
}

type fakeProbe struct{}

func (fakeProbe) Tag(t *config.ResolvedTemplate) (string, error) {
	return "faber/" + t.Name + ":20be39bb9bb7", nil
}
func (fakeProbe) Exists(context.Context, string) (bool, error) { return false, nil }
