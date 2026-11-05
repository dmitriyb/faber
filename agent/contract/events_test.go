package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Verifies 3fe064781899: the events file is read as the operator's hooks
// write it — ts, hook, event and opaque string fields — and every line
// that does not fit the shape comes back malformed with its line number,
// never dropped.
func TestReadEventsAcceptsRealEmitShapes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HooksEventsFile)
	os.MkdirAll(filepath.Dir(path), 0o755)
	lines := []string{
		`{"ts":"2026-11-04T10:00:00Z","hook":"post-review","event":"decision","pr":"17","result":"landable","mss":"clean"}`,
		``,
		`{"ts":"2026-11-04T10:00:05Z","hook":"post-review","event":"halt","reason":"approval-not-boolean","pr":"17","next":"clear the reservation on the PR, then resume"}`,
		`not json`,
		`{"ts":"2026-11-04T10:00:06Z","hook":"post-review","event":"push_verify","ref":"work/x","result":"ok","n":3}`,
		`{"ts":"yesterday","hook":"post-review","event":"x"}`,
		`{"ts":"2026-11-04T10:00:07Z","hook":"","event":"x"}`,
		`{"ts":"2026-11-04T10:00:07Z","hook":"h"}`,
		`[1,2]`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 || got.Truncated {
		t.Fatalf("want two accepted events, got %+v", got)
	}
	d := got.Events[0]
	if d.Line != 1 || d.Hook != "post-review" || d.Name != "decision" || d.Fields["pr"] != "17" || d.Fields["mss"] != "clean" || d.TS.IsZero() {
		t.Errorf("decision event: %+v", d)
	}
	if keys := d.SortedFieldKeys(); strings.Join(keys, ",") != "mss,pr,result" {
		t.Errorf("field order: %v", keys)
	}
	h := got.Events[1]
	if h.Line != 3 || h.Name != EventHalt || h.Fields[EventNext] != "clear the reservation on the PR, then resume" {
		t.Errorf("halt event: %+v", h)
	}
	wantBad := map[int]string{4: "not a JSON object", 5: `field "n" is not a string`, 6: "not RFC 3339", 7: "no hook", 8: "no event", 9: "not a JSON object"}
	if len(got.Malformed) != len(wantBad) {
		t.Fatalf("malformed lines: %+v", got.Malformed)
	}
	for _, m := range got.Malformed {
		if !strings.Contains(m.Err, wantBad[m.Line]) {
			t.Errorf("line %d: %q, want %q", m.Line, m.Err, wantBad[m.Line])
		}
	}
}

// Verifies 3fe064781899: a missing file is no events; an oversize file
// yields its newest lines with true line numbers and says it was cut.
func TestReadEventsMissingAndBounded(t *testing.T) {
	got, err := ReadEvents(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil || len(got.Events) != 0 || len(got.Malformed) != 0 {
		t.Fatalf("missing file: %+v %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	line := `{"ts":"2026-11-04T10:00:00Z","hook":"h","event":"progress","detail":"` + strings.Repeat("x", 200) + `"}` + "\n"
	n := MaxEventsBytes/len(line) + 50
	if err := os.WriteFile(path, []byte(strings.Repeat(line, n)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Malformed) != 0 || len(got.Events) == 0 || len(got.Events) >= n {
		t.Fatalf("bounded read: truncated=%v events=%d malformed=%d of %d lines", got.Truncated, len(got.Events), len(got.Malformed), n)
	}
	if last := got.Events[len(got.Events)-1]; last.Line != n {
		t.Errorf("last line numbered %d, want %d", last.Line, n)
	}
	if first := got.Events[0]; first.Line != n-len(got.Events)+1 {
		t.Errorf("first line numbered %d, want %d", first.Line, n-len(got.Events)+1)
	}
}
