package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
