package tui

import (
	"context"
	"fmt"
	"github.com/dmitriyb/faber/config"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/pipeline"
)

// Verifies f392ecd71cd1 and 3fe064781899: STEP shows the attempt's hooks
// events as the hooks wrote them — writer, event, fields as key=value, a
// failed result and a malformed line called out — and a halt event's next
// sentence takes the place of `resume` on the now strip.
func TestStepShowsHookEvents(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	m.sel = 0
	m.handle(context.Background(), key{r: 'n'})
	frame := m.render(221, 51).plain()
	for _, want := range []string{
		"hooks   attempt 1   2 events   1 malformed line",
		"fetch-change", "push_verify", "ref=work/x  result=ok",
		"checks_read", "attempts=3  pr=17  result=failed",
		"malformed  line 3: not a JSON object",
		"failed at review-cycle 2/3 > review   next: resume",
	} {
		if !strings.Contains(frame, want) {
			t.Errorf("STEP lacks %q:\n%s", want, frame)
		}
	}

	events := filepath.Join(pipeline.ResultDir(store.RunDir("20260902-170408-d0feb66b"), "task/review-cycle@2/review", 1), "hooks", "events.jsonl")
	f, err := os.OpenFile(events, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"ts":"2026-09-02T19:17:00Z","hook":"post-review","event":"halt","reason":"approval-not-boolean","next":"clear the reservation on PR 17, then resume"}` + "\n")
	f.Close()
	frame = m.render(221, 51).plain()
	for _, want := range []string{"next: clear the reservation on PR 17, then resume", "post-review", "halt", "reason=approval-not-boolean"} {
		if !strings.Contains(frame, want) {
			t.Errorf("after a halt event, STEP lacks %q:\n%s", want, frame)
		}
	}

	// a step no hook wrote about has no hooks block
	m.openRun("20260915-180538-4fc8c4f3")
	m.sel = 0
	if frame := m.render(221, 51).plain(); strings.Contains(frame, "hooks   attempt") {
		t.Errorf("live run's implement has no events file, yet:\n%s", frame)
	}
}

// errProbe answers every tag with one error.
type errProbe struct{ err error }

func (p errProbe) Tag(*config.ResolvedTemplate) (string, error) { return "", p.err }
func (errProbe) Exists(context.Context, string) (bool, error)   { return false, nil }

// Verifies f392ecd71cd1: a missing baked hook is one problem on Home — the
// templates row names the slot and the image row reads not checked — while
// a tag failure of another kind is FAIL with its reason, the template
// prefix dropped.
func TestImageRowWhenTheTagCannotBeComputed(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	abs, _ := filepath.Abs(refConfig)
	m := newApp(Options{Store: store, Now: func() time.Time { return fixed }, Images: errProbe{err: fmt.Errorf("%w: hooks.prelude not found", ErrImageInputs)}}, config.UIOptions{ConfigPath: abs})
	m.refresh(context.Background())
	frame := m.render(221, 51).plain()
	if !strings.Contains(frame, "images       not checked  4 of 4, a hook is missing") || strings.Contains(frame, "images       FAIL") {
		t.Errorf("a hook problem must read not checked with its reason, not FAIL:\n%s", frame)
	}
	if !strings.Contains(frame, "hooks        MISSING      5 slots in 3 templates") || !strings.Contains(frame, "implement/context   ./hooks/gather-context") {
		t.Errorf("the hooks class must still name the slot:\n%s", frame)
	}

	m2 := newApp(Options{Store: store, Now: func() time.Time { return fixed }, Images: errProbe{err: fmt.Errorf("templates.implement: infra: no faber-box binary to bake")}}, config.UIOptions{ConfigPath: abs})
	m2.refresh(context.Background())
	frame = m2.render(221, 51).plain()
	if !strings.Contains(frame, "images       FAIL         4 of 4") || !strings.Contains(frame, "implement           infra: no faber-box binary") {
		t.Errorf("tag failures must be counted as FAIL on the class line:\n%s", frame)
	}
	for _, ti := range m2.project.Templates {
		if ti.Name == "implement" && ti.Err != "infra: no faber-box binary to bake" {
			t.Errorf("implement's reason = %q, want the template prefix dropped", ti.Err)
		}
	}
}

// Verifies 1d5d7c4db2fe: the checklist drills down — enter on a class line
// shows the crumb and every problem with its fix, j moves, esc comes back
// with the cursor on the class.
func TestPreflightDrillDown(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.focus = "workflows"
	m.wfSel = len(m.project.Workflows) + 2 // config, images, keys
	frame := m.render(221, 51).plain()
	if !strings.Contains(frame, "keys         MISSING") {
		t.Fatalf("keys line missing:\n%s", frame)
	}
	m.handle(context.Background(), key{name: "enter"})
	if m.pfDrill != "keys" {
		t.Fatalf("enter on the keys line did not drill in: %q", m.pfDrill)
	}
	frame = m.render(221, 51).plain()
	for _, want := range []string{"PREFLIGHT > keys   MISSING   3 of 3 roles", "who", "implementer", "j/k move   esc/<- back   q quit"} {
		if !strings.Contains(frame, want) {
			t.Errorf("drill-down lacks %q:\n%s", want, frame)
		}
	}
	if !regexp.MustCompile(`reviewer\s+key file not found`).MatchString(frame) {
		t.Errorf("drill-down lacks the reviewer row:\n%s", frame)
	}
	m.handle(context.Background(), key{r: 'j'})
	m.handle(context.Background(), key{r: 'j'})
	if m.pfSel != 2 {
		t.Errorf("j twice: cursor %d", m.pfSel)
	}
	m.handle(context.Background(), key{name: "esc"})
	if m.pfDrill != "" || m.wfSel != len(m.project.Workflows)+2 || m.screen != "home" {
		t.Errorf("esc did not come back to the class: drill %q wfSel %d screen %q", m.pfDrill, m.wfSel, m.screen)
	}
	m.handle(context.Background(), key{name: "right"})
	if m.pfDrill != "keys" {
		t.Errorf("right on the keys line did not drill in: %q", m.pfDrill)
	}
	// a narrow pane folds the fix into the what column; the reason stays
	narrow := m.render(80, 24).plain()
	if !regexp.MustCompile(`PREFLIGHT > keys.*\n(.*\n)*.*implementer\s+key file not found`).MatchString(narrow) {
		t.Errorf("80-column drill-down lost the reason:\n%s", narrow)
	}
	m.handle(context.Background(), key{name: "left"})
	if m.pfDrill != "" {
		t.Errorf("left did not come back: %q", m.pfDrill)
	}
	if !regexp.MustCompile(`\n\s{4}implementer\s+key file not found`).MatchString(m.render(80, 24).plain()) {
		t.Errorf("80-column checklist row lost the reason:\n%s", m.render(80, 24).plain())
	}
	// a project that does not load has nothing to drill into
	m.project.Err = fmt.Errorf("no such config")
	m.handle(context.Background(), key{name: "enter"})
	if m.pfDrill != "" {
		t.Errorf("enter on a failed project drilled into %q", m.pfDrill)
	}
}

// daemonProbe computes tags but cannot ask the daemon.
type daemonProbe struct{}

func (daemonProbe) Tag(*config.ResolvedTemplate) (string, error) {
	return "sha256:" + strings.Repeat("ab", 32), nil
}
func (daemonProbe) Exists(context.Context, string) (bool, error) {
	return false, fmt.Errorf("docker unreachable: connect: no such file")
}

// Verifies f392ecd71cd1: an unreachable daemon leaves the images not
// checked with the probe's reason on the row, never FAIL or MISSING.
func TestImageRowWhenTheDaemonIsUnreachable(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	abs, _ := filepath.Abs(refConfig)
	m := newApp(Options{Store: store, Now: func() time.Time { return fixed }, Images: daemonProbe{}}, config.UIOptions{ConfigPath: abs})
	m.refresh(context.Background())
	frame := m.render(221, 51).plain()
	if !strings.Contains(frame, "images       not checked  4 of 4, docker unreachable") || !regexp.MustCompile(`implement\s+docker unreachable: connect`).MatchString(frame) {
		t.Errorf("an unreachable daemon must read not checked with its reason:\n%s", frame)
	}
	if strings.Contains(frame, "images       FAIL") || strings.Contains(frame, "images       MISSING") {
		t.Errorf("an unreachable daemon is neither FAIL nor MISSING:\n%s", frame)
	}
}

// Verifies 1d5d7c4db2fe: STEP and OUTPUT scroll — tab focuses STEP, j drops
// rows from its top and the title says how many; tab again focuses OUTPUT,
// k walks back from the tail; esc returns the focus to the list; moving the
// list resets both; the narrow footer still ends in q back.
func TestStepAndOutputScroll(t *testing.T) {
	store, release := seedStore(t)
	defer release()
	m := testModel(t, store)
	m.openRun("20260902-170408-d0feb66b")
	m.sel = 0
	m.handle(context.Background(), key{r: 'n'}) // the failed review: error, handoff, events
	// a long output for the window to scroll in
	dir := pipeline.AttemptDir(store.RunDir("20260902-170408-d0feb66b"), "task/review-cycle@2/review", 1)
	var sb strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, pipeline.OutputLogName), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	m.handle(context.Background(), key{r: '\t'})
	if m.rfocus != "step" {
		t.Fatalf("tab did not focus STEP: %q", m.rfocus)
	}
	for i := 0; i < 3; i++ {
		m.handle(context.Background(), key{r: 'j'})
	}
	frame := m.render(221, 51).plain()
	if m.soff != 3 || !strings.Contains(frame, " STEP    3 above") {
		t.Errorf("STEP did not scroll: soff %d\n%s", m.soff, frame)
	}
	if !strings.Contains(frame, "j/k scroll   pgup/pgdn page   tab next pane   esc/<- list   q back") {
		t.Errorf("footer for a focused pane:\n%s", frame)
	}
	m.handle(context.Background(), key{r: '\t'})
	if m.rfocus != "output" {
		t.Fatalf("tab did not focus OUTPUT: %q", m.rfocus)
	}
	m.handle(context.Background(), key{r: 'k'})
	m.handle(context.Background(), key{r: 'k'})
	frame = m.render(221, 51).plain()
	if m.ooff != 2 || !strings.Contains(frame, " OUTPUT    attempt 1   output.log") || !strings.Contains(frame, "above") || !strings.Contains(frame, "line 58") || strings.Contains(frame, "line 60") {
		t.Errorf("OUTPUT did not scroll back two lines: ooff %d\n%s", m.ooff, frame)
	}
	m.handle(context.Background(), key{name: "esc"})
	if m.rfocus != "" {
		t.Errorf("esc did not return the focus to the list: %q", m.rfocus)
	}
	m.handle(context.Background(), key{r: 'k'})
	m.render(221, 51)
	if m.soff != 0 || m.ooff != 0 {
		t.Errorf("moving the list did not reset the offsets: %d %d", m.soff, m.ooff)
	}
	m.handle(context.Background(), key{r: '\t'})
	if frame := m.render(80, 24).plain(); !strings.HasSuffix(strings.TrimRight(frame, "\n "), "q back") {
		t.Errorf("narrow footer with a focused pane does not end in q back:\n%s", frame)
	}
	// q from a focused pane leaves the run as the list's q does: with the
	// re-enter command for a failed run
	m.handle(context.Background(), key{r: 'q'})
	if m.screen != "home" || m.rfocus != "" || !strings.HasPrefix(m.quitCmd, "faber resume "+m.run.ID) {
		t.Errorf("q from a pane: screen %q focus %q quitCmd %q", m.screen, m.rfocus, m.quitCmd)
	}
}
