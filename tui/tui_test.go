package tui

import (
	"context"
	"encoding/json"
	"fmt"
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
		for _, view := range []string{"tree", "flow", "timeline"} {
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
	in := strings.NewReader("j\x1b[A\r\x1b[6~\x1bq")
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
	if want := "j up enter pgdn esc q"; strings.Join(got, " ") != want {
		t.Errorf("keys %q, want %q", strings.Join(got, " "), want)
	}
}

// Verifies 1d5d7c4db2fe: the flow view reads the IR as the author wrote it
// — steps in dependency order, guards as "if …" with leaf names, loops as
// frames with cap and exit — and on a run the current iteration carries
// the journal's state while the cursor skips frame edges.
func TestFlowRows(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	m.view = "flow"
	var lines []string
	for _, f := range m.frows {
		lines = append(lines, fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s", f.depth, f.kind, f.name, f.cap, f.guard, f.status, f.after))
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{
		"0|step|implement|||ok|",
		"0|open|review-cycle|2 of up to 3|until review.verdict == \"approved\"||",
		"1|step|review|||FAIL|",
		"1|step|fix||if review.verdict == \"changes\"|not reached|",
		"0|close|",
		"0|step|merge||if review.verdict == \"approved\"|not reached|",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("flow rows lack %q:\n%s", want, got)
		}
	}
	m.fsel = 0
	m.handle(context.Background(), key{r: 'j'})
	if f := m.frows[m.fsel]; f.kind != "step" || f.name != "review" {
		t.Errorf("j landed on %+v, want the review step (frame edges are skipped)", f)
	}
	if r := m.selectedRow(); r == nil || r.id != "task/review-cycle@2/review" {
		t.Errorf("STEP follows the flow cursor: %+v", r)
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
	checkGolden(t, "workflow-flow-221x51.txt", m.render(221, 51).plain())
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
	if !strings.Contains(frame, "images      4    4 MISSING") || !strings.Contains(frame, "merge            faber build   20be39bb9bb7") {
		t.Errorf("missing images: expected the count and a row per image with its tag hash:\n%s", frame)
	}
}

type fakeKeys map[string]string // role -> key source; absent = not in the registry

func (k fakeKeys) Resolve(_ context.Context, role string, _ config.IdentityDef) (string, error) {
	if src, ok := k[role]; ok {
		return src, nil
	}
	return "", fmt.Errorf("identity %q: role not in registry", role)
}

// Verifies 1d5d7c4db2fe: Home's preflight is a count per class with a
// row per problem — a role is MISSING only when a template uses it and no
// key resolves, a declared role no template names is unused, a hook path
// that is not there is MISSING, the resolver is checked by path — and the
// same checks say ok once the files exist.
func TestPreflight(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	// the reference config names hooks and keys that do not exist beside it
	abs, _ := filepath.Abs(refConfig)
	m := newApp(Options{Store: store, Now: func() time.Time { return fixed }}, config.UIOptions{ConfigPath: abs})
	m.refresh(context.Background())
	pf := m.project.Preflight
	if len(pf.Roles) != 3 || pf.Roles[0].State != "MISSING" || pf.Roles[0].Fix != "key file not found" {
		t.Errorf("path-form roles without files: %+v", pf.Roles)
	}
	if pf.Templates != 4 || len(pf.Slots) != 5 || pf.Slots[0].Name != "fix" || pf.Slots[0].Detail != "context" || pf.Slots[0].Fix != "./hooks/fetch-change" {
		t.Errorf("slots without files: %d templates, %+v", pf.Templates, pf.Slots)
	}
	if pf.Resolver.State != "MISSING" || pf.Resolver.Detail != "./hooks/get-token" {
		t.Errorf("resolver: %+v", pf.Resolver)
	}
	frame := m.render(221, 51).plain()
	for _, want := range []string{"identities  3    3 MISSING", "implementer      key file not found", "templates   4    3 MISSING", "implement   prelude    ./hooks/claim-item", "resolver MISSING  ./hooks/get-token   1 service"} {
		if !strings.Contains(frame, want) {
			t.Errorf("Home lacks %q:\n%s", want, frame)
		}
	}
	// a copy of the config with the files present, roles by registry
	dir := t.TempDir()
	src, _ := os.ReadFile(refConfig)
	text := string(src)
	text = strings.ReplaceAll(text, "implementer: {key: ./keys/implementer}", "implementer: {}")
	text = strings.ReplaceAll(text, "merger:      {key: ./keys/merger}", "merger:      {}\n  auditor:     {}")
	cfg := filepath.Join(dir, "orchestrator.yaml")
	if err := os.WriteFile(cfg, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"hooks/gather-context", "hooks/claim-item", "hooks/release-item", "hooks/fetch-change", "hooks/get-token", "keys/reviewer"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, f), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// a root config's paths are CWD-relative, for the view as for the run
	t.Chdir(dir)
	m2 := newApp(Options{Store: store, Now: func() time.Time { return fixed }, Keys: fakeKeys{"implementer": "~/.ssh/id_impl"}}, config.UIOptions{ConfigPath: cfg})
	m2.refresh(context.Background())
	if m2.project.Err != nil {
		t.Fatalf("copied config: %v", m2.project.Err)
	}
	got := map[string]Check{}
	for _, c := range m2.project.Preflight.Roles {
		got[c.Name] = c
	}
	if got["implementer"].State != "ok" || got["implementer"].Detail != "registry  ~/.ssh/id_impl" {
		t.Errorf("registry role with a key: %+v", got["implementer"])
	}
	if got["reviewer"].State != "ok" || got["reviewer"].Detail != "./keys/reviewer" {
		t.Errorf("path role with its file: %+v", got["reviewer"])
	}
	if got["merger"].State != "MISSING" || got["merger"].Fix != "faber add-key --role merger" {
		t.Errorf("registry role without a key: %+v", got["merger"])
	}
	if got["auditor"].State != "unused" {
		t.Errorf("a role no template names: %+v", got["auditor"])
	}
	if len(m2.project.Preflight.Slots) != 0 || m2.project.Preflight.Templates != 4 {
		t.Errorf("templates with their files: %+v", m2.project.Preflight)
	}
	if m2.project.Preflight.Resolver.State != "ok" {
		t.Errorf("resolver with its file: %+v", m2.project.Preflight.Resolver)
	}
	frame = m2.render(221, 51).plain()
	for _, want := range []string{"identities  4    1 MISSING", "merger           faber add-key --role merger", "auditor          unused", "templates   4    ok", "resolver ok   1 service   not checked"} {
		if !strings.Contains(frame, want) {
			t.Errorf("Home lacks %q:\n%s", want, frame)
		}
	}
}

// Verifies 1d5d7c4db2fe: the preview's right pane is blocks that start
// folded — STEP open, the rest one line — with NEEDS for this workflow;
// tab gives the pane the cursor, j/k walk the block heads, enter opens or
// closes, a block with a problem opens itself once and then folds when
// told to, and the STEP block names all four hooks.
func TestPreviewFolds(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openWorkflow("task")
	frame := m.render(221, 51).plain()
	for _, want := range []string{"> PARAMS   2 params", "> STRUCTURE   1 loop", "v NEEDS   3 roles   4 templates   1 service   9 MISSING", "v STEP   implement   template implement   identity implementer", "v hooks   3 of 4 set   3 MISSING", "postlude    none", "> inputs/outputs   2 inputs   2 outputs", "> history   2 attempts   p50 3m00s   all runs of task"} {
		if !strings.Contains(frame, want) {
			t.Errorf("preview pane lacks %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "    item        string   required") {
		t.Errorf("PARAMS must start folded:\n%s", frame)
	}
	m.handle(context.Background(), key{r: '\t'})
	if !m.pfocus {
		t.Fatal("tab did not focus the pane")
	}
	m.handle(context.Background(), key{name: "enter"}) // PARAMS open
	frame = m.render(221, 51).plain()
	if !strings.Contains(frame, "v PARAMS   2 params") || !strings.Contains(frame, "item        string   required") {
		t.Errorf("enter did not open PARAMS:\n%s", frame)
	}
	if !strings.Contains(frame, "tab/<- flow") {
		t.Errorf("footer does not say how to leave the pane:\n%s", frame)
	}
	m.handle(context.Background(), key{r: 'j'})
	m.handle(context.Background(), key{r: 'j'}) // NEEDS
	m.handle(context.Background(), key{name: "enter"})
	frame = m.render(221, 51).plain()
	if !strings.Contains(frame, "> NEEDS") || strings.Contains(frame, "identity    merger") {
		t.Errorf("a problem block folds when told to:\n%s", frame)
	}
	m.handle(context.Background(), key{r: '\t'})
	if m.pfocus {
		t.Fatal("tab did not return to the flow")
	}
	m.handle(context.Background(), key{r: 'j'}) // the flow cursor moves again
	if r := m.selectedRow(); r == nil || r.name != "review" {
		t.Errorf("flow cursor after tab back: %+v", r)
	}
}

type fakeProbe struct{}

func (fakeProbe) Tag(t *config.ResolvedTemplate) (string, error) {
	return "faber/" + t.Name + ":20be39bb9bb7", nil
}
func (fakeProbe) Exists(context.Context, string) (bool, error) { return false, nil }

// Verifies 1d5d7c4db2fe: the timeline has its own cursor over its rows,
// STEP follows the selected row, a short step keeps its letter at a coarse
// scale, +/- zoom the columns, and enter drills into an iteration: its
// steps become the rows on the iteration's own axis, the breadcrumb names
// the path, esc comes back up with the cursor where it was.
func TestTimelineCursorAndLetters(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	m.view = "timeline"
	m.follow = false
	names := func() string {
		var g []string
		for _, r := range m.trows {
			g = append(g, r.name)
		}
		return strings.Join(g, " ")
	}
	if got := names(); got != "implement review-cycle@1 review-cycle@2" {
		t.Fatalf("timeline rows %q", got)
	}
	m.tsel = 0
	m.handle(context.Background(), key{r: 'j'})
	m.handle(context.Background(), key{r: 'j'})
	if r := m.selectedRow(); r == nil || r.kind != "iter" || r.id != "task/review-cycle@2/" {
		t.Errorf("timeline selection %+v, want iteration 2", r)
	}
	frame := m.render(120, 40).plain()
	if !strings.Contains(frame, "> review-cycle@2") || !strings.Contains(frame, "whole run") {
		t.Errorf("selected row or the top-level breadcrumb missing:\n%s", frame)
	}
	// at 12 columns the 3-minute implement shares a column with the review
	// that starts right after it; the earlier letter must survive
	narrow := m.render(34, 40).plain()
	if !strings.Contains(narrow, "implement") {
		t.Fatalf("narrow frame:\n%s", narrow)
	}
	for _, line := range strings.Split(narrow, "\n") {
		if strings.Contains(line, "implement") && strings.Contains(line, "i") && !strings.Contains(line[2:], "i") {
			t.Errorf("implement lost its letter at a coarse scale: %q", line)
		}
	}
	m.handle(context.Background(), key{r: '+'})
	if m.tzoom != 1 {
		t.Errorf("+ did not zoom: %d", m.tzoom)
	}
	zoomed := m.render(221, 51).plain()
	if !strings.Contains(zoomed, "zoom x2") {
		t.Errorf("zoom not reflected:\n%s", zoomed)
	}
	m.handle(context.Background(), key{r: '-'})
	if m.tzoom != 0 {
		t.Errorf("- did not zoom out: %d", m.tzoom)
	}
	// drill into iteration 2: its executed step is the only row, on its own axis
	m.handle(context.Background(), key{name: "enter"})
	if m.view != "timeline" || len(m.tpath) != 1 || m.tpath[0] != "task/review-cycle@2/" {
		t.Fatalf("enter did not drill in: view %q path %v", m.view, m.tpath)
	}
	if got := names(); got != "review" {
		t.Fatalf("rows inside iteration 2: %q", got)
	}
	if r := m.selectedRow(); r == nil || r.kind != "step" || r.id != "task/review-cycle@2/review" {
		t.Errorf("selection inside %+v, want the review step", r)
	}
	frame = m.render(120, 40).plain()
	if !strings.Contains(frame, "TIMELINE   review-cycle 2/3") || !strings.Contains(frame, "in task") || !strings.Contains(frame, "review-cycle 2/3 > review") {
		t.Errorf("breadcrumb, parent bar or STEP missing inside the iteration:\n%s", frame)
	}
	if !strings.Contains(frame, "-> in   <- up") {
		t.Errorf("the timeline footer does not name the level keys:\n%s", frame)
	}
	m.handle(context.Background(), key{name: "left"})
	if m.screen != "run" || len(m.tpath) != 0 || m.tsel != 2 {
		t.Errorf("left did not come back up to iteration 2: screen %q path %v cursor %d", m.screen, m.tpath, m.tsel)
	}
	m.handle(context.Background(), key{name: "right"})
	if len(m.tpath) != 1 {
		t.Fatalf("right did not drill in: path %v", m.tpath)
	}
	m.handle(context.Background(), key{name: "esc"})
	if len(m.tpath) != 0 || m.tsel != 2 {
		t.Errorf("esc did not come back up: path %v cursor %d", m.tpath, m.tsel)
	}
	m.handle(context.Background(), key{name: "esc"})
	if m.screen != "home" {
		t.Errorf("esc at the top level did not leave the run: screen %q", m.screen)
	}
}

// Verifies f392ecd71cd1: the store's history is a distribution per step
// name — p50, p90, p99 and the sample count — shown on a run's STEP and in
// the preview's rows.
func TestStatsDistribution(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	// only ok attempts count: the failed 3-minute review is not history
	s, ok := m.stats["task"]["review"]
	if !ok || s.N != 1 || s.P50 != 4*time.Minute || s.P90 != 4*time.Minute || s.P99 != 4*time.Minute {
		t.Fatalf("review stats %+v, want n=1 p50=p90=p99=4m", s)
	}
	m.openRun("20260902-170408-d0feb66b")
	m.follow = true
	m.rebuildRows()
	frame := m.render(221, 51).plain()
	if !strings.Contains(frame, "history   1 attempt   all runs of task   h to switch") || !strings.Contains(frame, "    p90     4m00s") {
		t.Errorf("STEP lacks the distribution:\n%s", frame)
	}
	// the scope is a toggle that says what it covers: implement ran ok in
	// both runs, once in this one
	m.follow = false
	m.sel = 0
	if r := m.selectedRow(); r == nil || r.name != "implement" {
		t.Fatalf("row 0 is %+v, want implement", r)
	}
	if frame := m.render(221, 51).plain(); !strings.Contains(frame, "history   2 attempts   all runs of task") {
		t.Errorf("all-runs scope for implement:\n%s", frame)
	}
	m.handle(context.Background(), key{r: 'h'})
	if m.run.Scope != ScopeThisRun || m.sel != 0 {
		t.Fatalf("h: scope %q cursor %d", m.run.Scope, m.sel)
	}
	if frame := m.render(221, 51).plain(); !strings.Contains(frame, "history   1 attempt   this run   h to switch") {
		t.Errorf("this-run scope for implement:\n%s", frame)
	}
	m.handle(context.Background(), key{r: 'h'})
	if m.run.Scope != ScopeAllRuns {
		t.Fatalf("h again: scope %q", m.run.Scope)
	}
	m.handle(context.Background(), key{r: 'q'})
	m.handle(context.Background(), key{r: '\t'})
	m.handle(context.Background(), key{r: 'j'})
	m.handle(context.Background(), key{name: "enter"})
	preview := m.render(221, 51).plain()
	if !strings.Contains(preview, "p50 4m00s  p90 4m00s") {
		t.Errorf("preview rows lack the history:\n%s", preview)
	}
}

// Verifies 1d5d7c4db2fe (edge): a tick rebuilds the rows without moving
// any cursor — the tree, flow and timeline selections stay where the
// operator put them.
func TestTickDoesNotMoveCursors(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	m.follow = false
	m.view = "tree"
	m.sel, m.fsel, m.tsel = 2, 1, 1
	for i := 0; i < 5; i++ {
		m.refresh(context.Background()) // what the one-hertz tick does
	}
	if m.sel != 2 || m.tsel != 1 {
		t.Errorf("tick moved cursors: tree %d (want 2), timeline %d (want 1)", m.sel, m.tsel)
	}
	if f := m.frows[m.fsel]; f.kind != "step" && f.kind != "gen" {
		t.Errorf("flow cursor on a frame edge: %+v", f)
	}
	before := m.fsel
	m.refresh(context.Background())
	if m.fsel != before {
		t.Errorf("tick moved the flow cursor from %d to %d", before, m.fsel)
	}
}

// Verifies a22951c98fe3: an action is a detached faber child — the
// spawned command, its arguments, and the log file the view names are
// what the operator can reproduce by hand.
func TestSpawnIsAFaberChild(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	exe := filepath.Join(dir, "faber")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\necho spawned\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(refConfig)
	m := newApp(Options{Store: store, Now: func() time.Time { return fixed }, Executable: exe, LogDir: filepath.Join(dir, "logs")},
		config.UIOptions{ConfigPath: abs})
	m.refresh(context.Background())
	status := m.spawn("run", "task", "--param", "repo=x")
	if !strings.HasPrefix(status, "started: faber run task --param repo=x --config "+abs+" --progress json") {
		t.Fatalf("status %q", status)
	}
	logPath := strings.TrimSuffix(strings.TrimPrefix(status[strings.Index(status, "(log "):], "(log "), ")")
	var args []byte
	for i := 0; i < 100; i++ { // the child is detached; wait for its output
		if b, err := os.ReadFile(argsFile); err == nil && len(b) > 0 {
			args = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if want := "run\ntask\n--param\nrepo=x\n--config\n" + abs + "\n--progress\njson\n"; string(args) != want {
		t.Errorf("child args %q, want %q", args, want)
	}
	for i := 0; i < 100; i++ {
		if b, _ := os.ReadFile(logPath); strings.Contains(string(b), "spawned") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("child output not in the named log %s", logPath)
}

// Verifies a22951c98fe3: resume on a live run is refused with a message —
// the run holds its own lock, and a child would only report that.
func TestResumeRefusedOnLive(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.runSel = 0 // the live run sorts first
	if m.selectedRun().State != "LIVE" {
		t.Fatalf("expected the live run selected, got %+v", m.selectedRun())
	}
	m.handle(context.Background(), key{r: 'R'})
	if !strings.Contains(m.status, "is live") {
		t.Errorf("resume on a live run: status %q", m.status)
	}
}

// Verifies 38b440a94d4c: Ctrl-C and Ctrl-D arrive as bytes in raw mode
// and end the view like q; a 9-row terminal and an empty store render
// without panicking.
func TestQuitKeysAndTinyScreens(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	for _, r := range []rune{0x03, 0x04} {
		if m.handle(context.Background(), key{r: r}) {
			t.Errorf("key %#x did not quit", r)
		}
	}
	m.openRun("20260902-170408-d0feb66b")
	for _, view := range []string{"tree", "flow", "timeline"} {
		m.view = view
		m.rebuildRows()
		for _, size := range [][2]int{{80, 9}, {40, 12}, {221, 51}} {
			if f := m.render(size[0], size[1]); len(f.rows) != size[1] {
				t.Errorf("%s at %dx%d: %d rows", view, size[0], size[1], len(f.rows))
			}
		}
	}
	empty := failure.NewStore(t.TempDir(), nil)
	e := testModel(t, empty)
	if f := e.render(80, 24); !strings.Contains(f.plain(), "no runs") {
		t.Errorf("empty store: expected 'no runs' on Home:\n%s", f.plain())
	}
	if f := e.render(80, 9); len(f.rows) != 9 {
		t.Errorf("empty store at 80x9: %d rows", len(f.rows))
	}
}

// Verifies f392ecd71cd1: a run whose config no longer loads still shows
// its journal — the tree from the report, the reason on the header strip
// — with the flow view honest about having no IR.
func TestRunWithoutConfig(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	// a third run whose header points at a config that is gone
	id := "20260920-100000-deadbeef"
	hdr := failure.Header{RunID: id, ConfigPath: filepath.Join(t.TempDir(), "gone.yaml"), Workflow: "task",
		Params: map[string]string{"repo": "sandbox", "item": "I-9"}, IRHash: "x", IRVersion: 1, Started: t0}
	j, err := store.Begin(hdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.AppendResult(failure.ResultRecord{StepID: "task/implement", InputHash: "h1", Result: okRes(t0, t0.Add(time.Minute), `{"branch":"b"}`)}); err != nil {
		t.Fatal(err)
	}
	m := testModel(t, store)
	m.openRun(id)
	if m.run == nil || m.run.IR != nil || m.run.Err == nil {
		t.Fatalf("expected a journal-only view with Err set, got IR=%v err=%v", m.run != nil && m.run.IR != nil, m.run != nil && m.run.Err != nil)
	}
	m.view = "tree"
	m.rebuildRows()
	plain := m.render(160, 30).plain()
	if !strings.Contains(plain, "implement") || !strings.Contains(plain, "config:") {
		t.Errorf("journal-only run: expected the step and the config reason:\n%s", plain)
	}
	m.view = "flow"
	m.rebuildRows()
	if plain := m.render(160, 30).plain(); !strings.Contains(plain, "nothing to show") {
		t.Errorf("flow without IR must say so:\n%s", plain)
	}
}

// Verifies f392ecd71cd1: the store is read by stat on a tick — a journal
// that did not grow is not replayed again, one that grew is, and the
// history follows the cache version.
func TestReplayCacheByStat(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	v1 := m.cache.version
	m.refresh(context.Background())
	if m.cache.version != v1 {
		t.Errorf("a quiet tick bumped the cache version %d -> %d", v1, m.cache.version)
	}
	id := "20260902-170408-d0feb66b"
	j, err := store.Reopen(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.AppendResult(failure.ResultRecord{StepID: "task/merge", InputHash: "h9", Result: okRes(fixed.Add(-time.Minute), fixed, `{"status":"done"}`)}); err != nil {
		t.Fatal(err)
	}
	m.refresh(context.Background())
	if m.cache.version == v1 {
		t.Error("an appended record did not bump the cache version")
	}
	if m.statsVer != m.cache.version {
		t.Errorf("stats at version %d, cache at %d", m.statsVer, m.cache.version)
	}
	if _, ok := m.stats["task"]["merge"]; !ok {
		t.Errorf("the new attempt is missing from the history: %v", m.stats)
	}
}

// Verifies 1d5d7c4db2fe: a long run drills down level by level — forty-five
// iterations, each holding a nested loop: enter on cycles@21 shows its two
// rows on its own axis, enter on the nested iteration its steps, every
// level with the full path above, and nothing overflows a short pane.
func TestTimelineDrillsDown(t *testing.T) {
	rp := &failure.Replay{Header: failure.Header{RunID: "r", Workflow: "epic", Started: t0}, Results: map[failure.Key]failure.ResultRecord{}}
	for i := 1; i <= 45; i++ {
		at := t0.Add(time.Duration(i) * 10 * time.Minute)
		for j, step := range []string{"implement", "review-cycle@1/review", "review-cycle@1/fix", "merge"} {
			id := fmt.Sprintf("epic/cycles@%d/%s", i, step)
			s := at.Add(time.Duration(j) * 2 * time.Minute)
			rp.Results[failure.Key{StepID: id, InputHash: "h"}] = failure.ResultRecord{StepID: id, InputHash: "h", Result: okRes(s, s.Add(90*time.Second), `{}`)}
		}
	}
	v := &RunView{ID: "r", Replay: rp, Report: &pipeline.RunReport{}, Bounds: map[string]int{"epic/cycles": 45, "epic/cycles@21/review-cycle": 3}}
	m := &model{st: style{}, now: func() time.Time { return fixed.Add(10 * time.Hour) }, screen: "run", run: v, view: "timeline", open: map[string]bool{}, alerted: map[string]bool{}}
	m.rebuildRows()
	if len(m.trows) != 45 || !m.trows[20].iter {
		t.Fatalf("%d top rows, want 45 iterations", len(m.trows))
	}
	m.tsel = 20
	m.handle(context.Background(), key{name: "enter"})
	names := func() string {
		var g []string
		for _, r := range m.trows {
			g = append(g, r.name)
		}
		return strings.Join(g, " ")
	}
	if got := names(); got != "implement review-cycle@1 merge" {
		t.Fatalf("inside cycles@21: %q", got)
	}
	m.handle(context.Background(), key{r: 'j'})
	m.handle(context.Background(), key{name: "enter"})
	if got := names(); got != "review fix" || len(m.tpath) != 2 {
		t.Fatalf("inside the nested iteration: %q, path %v", got, m.tpath)
	}
	out := stripSGR(strings.Join(timelinePane(m.st, m, 100, 24), "\n"))
	if !strings.Contains(out, "cycles 21/45 > review-cycle 1/3") || !strings.Contains(out, "in cycles@21") {
		t.Errorf("full path or parent bar missing:\n%s", out)
	}
	if n := strings.Count(out, "\n") + 1; n > 24 {
		t.Errorf("pane overflows: %d rows", n)
	}
	m.handle(context.Background(), key{r: 0x7f}) // backspace: one level up
	m.handle(context.Background(), key{r: 0x7f})
	if len(m.tpath) != 0 || m.tsel != 20 {
		t.Errorf("two levels up: path %v cursor %d (want 20)", m.tpath, m.tsel)
	}
}

// Verifies 1d5d7c4db2fe: one key vocabulary on every screen — arrows and
// tab move between panes on Home and the preview, right goes into the
// thing under the cursor and left comes out on the tree, esc backs out
// one step before it leaves a screen, page keys work on every list, and
// pause/resume act on the run on screen.
func TestKeysAreOneVocabulary(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	// Home: left is the project pane, right the runs pane; page keys clamp
	m.handle(context.Background(), key{name: "left"})
	if m.focus != "workflows" {
		t.Errorf("left on Home: focus %q", m.focus)
	}
	m.handle(context.Background(), key{name: "pgdn"})
	if m.wfSel != len(m.project.Workflows)-1 {
		t.Errorf("pgdn on the workflows: %d", m.wfSel)
	}
	m.handle(context.Background(), key{name: "right"})
	if m.focus != "runs" {
		t.Errorf("right on Home: focus %q", m.focus)
	}
	// preview: right goes to details, left comes back, esc from the pane comes back first
	m.openWorkflow("task")
	m.handle(context.Background(), key{name: "right"})
	if !m.pfocus {
		t.Fatal("right did not focus the details pane")
	}
	m.handle(context.Background(), key{name: "esc"})
	if m.pfocus || m.screen != "run" {
		t.Errorf("esc from the pane: focus %v screen %q (want the flow, still on the preview)", m.pfocus, m.screen)
	}
	m.handle(context.Background(), key{name: "right"})
	m.handle(context.Background(), key{name: "left"})
	if m.pfocus {
		t.Error("left did not return to the flow")
	}
	m.handle(context.Background(), key{name: "esc"})
	if m.screen != "home" {
		t.Errorf("esc on the flow did not leave the preview: %q", m.screen)
	}
	// tree: right opens an iteration, left closes it (from inside too)
	m.openRun("20260902-170408-d0feb66b")
	m.follow = false
	m.view = "tree"
	m.sel = 0
	for i := range m.rows {
		if m.rows[i].kind == "iter" && m.rows[i].id == "task/review-cycle@1/" {
			m.sel = i
		}
	}
	before := len(m.rows)
	m.handle(context.Background(), key{name: "right"})
	if len(m.rows) != before+2 || !m.open["task/review-cycle@1/"] {
		t.Fatalf("right did not open iteration 1: %d rows (was %d)", len(m.rows), before)
	}
	m.handle(context.Background(), key{r: 'j'}) // inside it
	m.handle(context.Background(), key{name: "left"})
	if len(m.rows) != before || m.rows[m.sel].id != "task/review-cycle@1/" {
		t.Errorf("left from inside did not close iteration 1 with the cursor on its row: %d rows, cursor on %q", len(m.rows), m.rows[m.sel].id)
	}
	// Run: p and R act on the run on screen
	m.handle(context.Background(), key{r: 'p'})
	if !strings.Contains(m.status, "not live") {
		t.Errorf("p on a settled run: %q", m.status)
	}
	m.openRun("20260915-180538-4fc8c4f3")
	m.handle(context.Background(), key{r: 'R'})
	if !strings.Contains(m.status, "is live") {
		t.Errorf("R on the live run: %q", m.status)
	}
	// a narrow terminal folds the footer to two rows instead of dropping keys
	m.view = "tree"
	f := m.render(80, 24)
	last, prev := f.plain(), ""
	rows := strings.Split(strings.TrimRight(last, "\n"), "\n")
	last, prev = rows[len(rows)-1], rows[len(rows)-2]
	if !strings.Contains(prev, "j/k move") || !strings.Contains(last, "q back") || m.footerRows != 2 {
		t.Errorf("narrow footer did not fold (%d rows):\n%s\n%s", m.footerRows, prev, last)
	}
}
