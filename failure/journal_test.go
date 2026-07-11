package failure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
)

func diamondHeader(runID string) Header {
	return Header{
		RunID:      runID,
		ConfigPath: "orchestrator.yaml",
		ConfigHash: "cfg-hash",
		Workflow:   "main",
		Params:     map[string]string{"target": "value"},
		IRHash:     "ir-hash",
		Started:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// Verifies bff0f92afc29: journal round trip over the diamond — the header
// carries run id, config path+hash, workflow, params, and IR hash; the
// replayed map reconstructs every node's terminal state; cost records exist
// for each executed step, keyed like the results.
func TestJournalRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	hdr := diamondHeader("run-1")
	j, err := store.Begin(hdr)
	if err != nil {
		t.Fatal(err)
	}

	// The diamond a→b, a→c, b+c→d with b failed: a ok, b failed, c ok; d is
	// skipped by fail-stop, so it settles no record.
	records := []ResultRecord{
		{StepID: "task/a", InputHash: "hash-a", Result: okResult(`{"field":"a-out"}`)},
		{StepID: "task/b", InputHash: "hash-b", Result: failedResult(ReasonAgent, "b died")},
		{StepID: "task/c", InputHash: "hash-c", Result: okResult(`{"field":"c-out"}`)},
	}
	for _, rec := range records {
		if err := j.AppendResult(rec); err != nil {
			t.Fatal(err)
		}
		// Every executed step — failed ones included — costs something; only
		// skipped steps emit no cost record.
		if err := j.AppendCost(CostRecord{StepID: rec.StepID, InputHash: rec.InputHash, Cost: json.RawMessage(`{"unit":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	rp, err := store.Load("run-1")
	if err != nil {
		t.Fatal(err)
	}
	wantHdr := hdr
	wantHdr.Kind = KindHeader
	if !reflect.DeepEqual(rp.Header, wantHdr) {
		t.Fatalf("header round trip:\nwant %+v\ngot  %+v", wantHdr, rp.Header)
	}
	if len(rp.Results) != 3 {
		t.Fatalf("want 3 result records, got %d", len(rp.Results))
	}
	if got := rp.Results[Key{"task/b", "hash-b"}]; got.Result.Status != StatusFailed || got.Result.Error.Reason != ReasonAgent {
		t.Fatalf("b's terminal state lost: %+v", got.Result)
	}
	if got := rp.Results[Key{"task/a", "hash-a"}]; string(got.Result.Payload) != `{"field":"a-out"}` {
		t.Fatalf("a's payload lost: %s", got.Result.Payload)
	}
	if _, ok := rp.Results[Key{"task/d", "hash-d"}]; ok {
		t.Fatal("skipped step must have no record")
	}
	if len(rp.Costs) != 3 {
		t.Fatalf("want cost records for all 3 executed (not skipped) steps, got %d", len(rp.Costs))
	}
}

// Verifies bff0f92afc29: the journal is append-only with last-wins replay — a
// resumed run's re-run appends a fresh record for the same key rather than
// editing history; both lines remain in the file, the later one wins.
func TestJournalAppendOnlyLastWins(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(diamondHeader("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	key := Key{StepID: "task/a", InputHash: "hash-a"}
	if err := j.AppendResult(ResultRecord{StepID: key.StepID, InputHash: key.InputHash, Result: failedResult(ReasonAgent, "first")}); err != nil {
		t.Fatal(err)
	}
	if err := j.AppendResult(ResultRecord{StepID: key.StepID, InputHash: key.InputHash, Result: okResult(`{"field":"second"}`)}); err != nil {
		t.Fatal(err)
	}
	j.Close()

	raw, err := os.ReadFile(filepath.Join(store.RunDir("run-1"), "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(raw), "\n"); lines != 3 { // header + 2 results
		t.Fatalf("append-only: want 3 lines, got %d:\n%s", lines, raw)
	}
	rp, err := store.Load("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := rp.Results[key]; got.Result.Status != StatusOK {
		t.Fatalf("later record must win: %+v", got.Result)
	}
}

// Verifies bff0f92afc29: a torn final line (crash mid-append) is dropped with
// a warning; everything before it replays intact, and the torn step reads as
// absent (so resume re-runs it).
func TestJournalTornLastLine(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(diamondHeader("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j.AppendResult(ResultRecord{StepID: "task/a", InputHash: "hash-a", Result: okResult(`{"field":1}`)}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	path := filepath.Join(store.RunDir("run-1"), "journal.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"result","step_id":"task/b","input_ha`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	rp, err := Load(path, nil)
	if err != nil {
		t.Fatalf("torn tail must not fail the load: %v", err)
	}
	if _, ok := rp.Results[Key{"task/a", "hash-a"}]; !ok {
		t.Fatal("intact record lost")
	}
	if _, ok := rp.LastByStep["task/b"]; ok {
		t.Fatal("torn record must read as absent")
	}
}

// Verifies bff0f92afc29: a malformed line that is not the final one is a hard
// error — silent corruption in the middle of history is never tolerated.
func TestJournalCorruptMiddleLineFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "journal.jsonl")
	hdrLine, _ := marshalLine(Header{Kind: KindHeader, RunID: "run-1"})
	content := string(hdrLine) + "{torn garbage\n" + `{"kind":"cost","step_id":"task/a","input_hash":"h","cost":{}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, nil); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want hard error naming line 2, got %v", err)
	}
}

// Verifies bff0f92afc29: unknown record kinds are skipped on replay (forward
// compatibility with newer journal writers).
func TestJournalUnknownKindSkipped(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(diamondHeader("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	path := filepath.Join(store.RunDir("run-1"), "journal.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"kind":"future-record","anything":true}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := Load(path, nil); err != nil {
		t.Fatalf("unknown kind must be skipped, got %v", err)
	}
}

// Verifies 3b7d2586b5ae and bff0f92afc29: the journal append boundary
// validates the record union — an invalid record never becomes durable state.
func TestJournalAppendValidates(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(diamondHeader("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	err = j.AppendResult(ResultRecord{StepID: "task/a", InputHash: "h", Result: Result{Status: StatusOK, Attempt: 1}})
	if err == nil || !strings.Contains(err.Error(), "requires a payload") {
		t.Fatalf("want validation error on append, got %v", err)
	}
}

// Verifies bff0f92afc29: concurrent step goroutines appending through one
// Journal never interleave lines — every line replays whole.
func TestJournalConcurrentAppends(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(diamondHeader("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	const n = 32
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := ResultRecord{
				StepID:    "task/" + strings.Repeat("x", i+1),
				InputHash: "h",
				Result:    okResult(`{"field":"` + strings.Repeat("y", 256) + `"}`),
			}
			if err := j.AppendResult(rec); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	j.Close()
	rp, err := store.Load("run-1")
	if err != nil {
		t.Fatalf("interleaved journal lines: %v", err)
	}
	if len(rp.Results) != n {
		t.Fatalf("want %d records, got %d", n, len(rp.Results))
	}
}

// Verifies fca4912f1bbe (first pass): one journal directory per run under the
// store root, no cross-run coordination — two runs are fully isolated, and a
// run id can only be begun once.
func TestStorePerRunIsolation(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root, nil)
	j1, err := store.Begin(diamondHeader("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	j1.Close()
	j2, err := store.Begin(diamondHeader("run-2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := j2.AppendResult(ResultRecord{StepID: "task/a", InputHash: "h", Result: okResult(`{}`)}); err != nil {
		t.Fatal(err)
	}
	j2.Close()

	if store.RunDir("run-1") == store.RunDir("run-2") {
		t.Fatal("runs must get distinct directories")
	}
	rp1, err := store.Load("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rp1.Results) != 0 {
		t.Fatalf("run-2's records leaked into run-1: %v", rp1.Results)
	}
	if _, err := store.Begin(diamondHeader("run-1")); err == nil {
		t.Fatal("beginning an existing run id must fail")
	}
}

// Verifies fca4912f1bbe (first pass): minted run ids are distinct, so per-run
// directories never collide.
func TestNewRunIDDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := NewRunID()
		if id == "" || seen[id] {
			t.Fatalf("run id collision or empty: %q", id)
		}
		seen[id] = true
	}
}

// Verifies bff0f92afc29: the Store satisfies the config CLI's JournalStore
// seam — LoadHeader returns the header fields resume re-derives the run from.
func TestLoadHeaderSeam(t *testing.T) {
	var _ config.JournalStore = (*Store)(nil)
	store := NewStore(t.TempDir(), nil)
	hdr := diamondHeader("run-1")
	j, err := store.Begin(hdr)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	got, err := store.LoadHeader("run-1")
	if err != nil {
		t.Fatal(err)
	}
	want := config.JournalHeader{
		RunID:      hdr.RunID,
		ConfigPath: hdr.ConfigPath,
		Workflow:   hdr.Workflow,
		Params:     hdr.Params,
		IRHash:     hdr.IRHash,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("seam header:\nwant %+v\ngot  %+v", want, got)
	}
	if _, err := store.LoadHeader("no-such-run"); err == nil {
		t.Fatal("missing run must error")
	}
}

// Verifies bff0f92afc29: an oversized record is refused at append time with a
// clear error — it must never become a journal line that replay's scanner
// cannot read back (which would poison every later Load).
func TestJournalAppendRejectsOversized(t *testing.T) {
	store := NewStore(t.TempDir(), nil)
	j, err := store.Begin(diamondHeader("run-1"))
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	j.limit = 1024 // lower the bound so the test stays cheap; semantics identical

	big := okResult(`{"field":"` + strings.Repeat("x", 2048) + `"}`)
	err = j.AppendResult(ResultRecord{StepID: "task/a", InputHash: "h", Result: big})
	if err == nil || !strings.Contains(err.Error(), "line limit") {
		t.Fatalf("want line-limit refusal, got %v", err)
	}
	// The journal is untouched by the refusal and still accepts sane records.
	if err := j.AppendResult(ResultRecord{StepID: "task/a", InputHash: "h", Result: okResult(`{"field":1}`)}); err != nil {
		t.Fatal(err)
	}
	rp, err := store.Load("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rp.Results) != 1 {
		t.Fatalf("want exactly the small record, got %d", len(rp.Results))
	}
}
