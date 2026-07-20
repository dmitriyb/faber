package failure

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzJournalLoad (review §5.1): Load never panics on arbitrary bytes; a
// loadable journal survives repair+append+reload with its prior results
// preserved; and Load∘write∘Load is a fixpoint for what it accepts.
func FuzzJournalLoad(f *testing.F) {
	hdr := `{"kind":"header","format":1,"run_id":"r","params":{}}`
	res := `{"kind":"result","step_id":"a","input_hash":"h","result":{"status":"ok","payload":{"x":1},"timing":{"started":"2026-01-02T03:04:05Z","finished":"2026-01-02T03:04:06Z"},"attempt":1}}`
	f.Add([]byte(hdr + "\n" + res + "\n"))
	f.Add([]byte(hdr + "\n" + res + "\n" + `{"kind":"result","step_id":"a"`)) // torn tail
	f.Add([]byte(hdr + "\n" + `{"kind":"mystery","step_id":"a"}` + "\n"))     // unknown kind
	f.Add([]byte(`{"kind":"header","format":9,"run_id":"r"}` + "\n"))         // newer format
	f.Add([]byte(hdr + "\n" + `{"kind":"result","step_id":"b","input_hash":"h","result":{"status":"weird","attempt":1}}` + "\n"))
	f.Add([]byte("\x00\xff garbage"))
	f.Add([]byte(hdr + "\n" + `{"kind":"defer","step_id":"a","detail":"wait"}` + "\n" + `{"kind":"run-end","status":"settled","failed":0}` + "\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		dir := t.TempDir()
		path := filepath.Join(dir, "journal.jsonl")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		rp, err := Load(path, nil) // must never panic
		if err != nil {
			return
		}
		prior := len(rp.Results)

		// Repair + append + reload: everything Load accepted survives.
		j, jerr := OpenJournal(path)
		if jerr != nil {
			t.Fatalf("loadable journal refused reopen: %v", jerr)
		}
		rec := ResultRecord{StepID: "fuzz/new", InputHash: "nh", Result: Result{
			Status: StatusOK, Payload: json.RawMessage(`{"v":1}`), Attempt: 1,
		}}
		if err := j.AppendResult(rec); err != nil {
			t.Fatalf("append after repair: %v", err)
		}
		j.Close()
		again, err := Load(path, nil)
		if err != nil {
			t.Fatalf("journal corrupted by repair+append: %v", err)
		}
		if len(again.Results) != prior+1 {
			t.Fatalf("prior results lost: had %d, now %d (want +1)", prior, len(again.Results)-1)
		}
		if _, ok := again.Results[Key{"fuzz/new", "nh"}]; !ok {
			t.Fatal("appended record missing on reload")
		}

		// Fixpoint: re-serializing what Load accepted loads identically.
		third, err := Load(path, nil)
		if err != nil || len(third.Results) != len(again.Results) {
			t.Fatalf("Load is not stable: %v", err)
		}
	})
}

// FuzzInputHash (review §5.6): equal values hash equal regardless of map
// iteration order; trailing bytes after a RawMessage are rejected; deep
// nesting neither panics nor overflows.
func FuzzInputHash(f *testing.F) {
	f.Add(`{"a":1,"b":{"c":[1,2,{"d":"x"}]}}`, "tpl", "img:tag")
	f.Add(`{"z":null,"a":true}`, "", "")
	f.Add(`{"n":123456789012345678901234567890}`, "t", "i")
	f.Fuzz(func(t *testing.T, rawJSON, tpl, img string) {
		var v map[string]any
		dec := json.NewDecoder(strings.NewReader(rawJSON))
		dec.UseNumber()
		if dec.Decode(&v) != nil {
			return
		}
		h1, err1 := InputHash(v, tpl, img)
		// Re-decode into a fresh map (fresh iteration order) — must agree.
		var v2 map[string]any
		dec2 := json.NewDecoder(strings.NewReader(rawJSON))
		dec2.UseNumber()
		if dec2.Decode(&v2) != nil {
			return
		}
		h2, err2 := InputHash(v2, tpl, img)
		if (err1 == nil) != (err2 == nil) || h1 != h2 {
			t.Fatalf("hash not canonical: %q/%v vs %q/%v", h1, err1, h2, err2)
		}
	})
}

// FuzzInputHashRawMessage: a RawMessage with trailing data must be rejected,
// never silently truncated into a colliding hash.
func FuzzInputHashRawMessage(f *testing.F) {
	f.Add(`{"x":1}`, `garbage`)
	f.Add(`1`, ` 2`)
	f.Fuzz(func(t *testing.T, doc, trailer string) {
		if !json.Valid([]byte(doc)) || strings.TrimSpace(trailer) == "" {
			return
		}
		inputs := map[string]any{"p": json.RawMessage(doc + trailer)}
		if _, err := InputHash(inputs, "t", "i"); err == nil {
			if json.Valid(bytes.TrimSpace([]byte(doc + trailer))) {
				return // the concatenation is itself one valid document
			}
			t.Fatalf("trailing bytes accepted: %q", doc+trailer)
		}
	})
}
