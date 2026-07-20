package pipeline

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent"
	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
)

func testRunResult() infra.RunResult {
	return infra.RunResult{ExitCode: 0, Started: testBase, Duration: 2e9}
}

// FuzzExtractAdapt (review §5.2): arbitrary bytes written as result.json,
// run through the real ExtractResult + adaptResult, must always yield a
// Validate-clean failure.Result, never panic; a threaded handoff pointer must
// resolve strictly under the attempt result dir; and no usage value survives
// negative.
func FuzzExtractAdapt(f *testing.F) {
	f.Add([]byte(`{"status":"ok","contract":1,"payload":{"out":"v"},"attempt":1}`), []byte(`{"tokens":5}`))
	f.Add([]byte(`{"status":"failed","contract":1,"error":{"reason":"agent-failed","detail":"x","handoff":"../../etc"},"attempt":1}`), []byte(`{"tokens":-9}`))
	f.Add([]byte(`{"status":"ok","payload":{"out":"v"},"attempt":1}`), []byte(`not json`))
	f.Add([]byte(`garbage`), []byte(``))
	f.Add([]byte(`{"status":"failed","contract":1,"error":{"reason":"skipped-condition","detail":"forge"},"attempt":1}`), []byte{})

	tpl := testTemplate("worker", "out")
	tpl.Env = map[string]string{contract.EnvAgentCLI: "cli"}
	tpl.Inputs = map[string]config.ParamDef{"out": {Type: "string", Required: true}}
	log := slog.New(slog.DiscardHandler)

	f.Fuzz(func(t *testing.T, record, usage []byte) {
		dir := t.TempDir()
		resultDir := filepath.Join(dir, "boxes", pathToken("w/x"), "attempt-1", "result")
		if err := os.MkdirAll(resultDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(resultDir, contract.ResultFile), record, 0o644); err != nil {
			t.Fatal(err)
		}
		if usage != nil && len(usage) > 0 {
			if err := os.WriteFile(filepath.Join(resultDir, contract.UsageFile), usage, 0o644); err != nil {
				t.Fatal(err)
			}
		}

		rec, err := agent.ExtractResult(resultDir, tpl.Output) // never panics
		if err != nil {
			return // an extract error is a legitimate outcome
		}
		box := BoxAttempt{RunID: "r", RunDir: dir, NodeID: "w/x", Attempt: 1, Template: tpl}
		out := adaptResult(rec, box, testRunResult(), log)

		if verr := out.Validate(); verr != nil {
			t.Fatalf("adaptResult produced an invalid Result: %v (from %q)", verr, record)
		}
		if out.Status == failure.StatusFailed && out.Error != nil && out.Error.Handoff != "" {
			base := filepath.Join("boxes", pathToken("w/x"), "attempt-1", "result")
			if !strings.HasPrefix(out.Error.Handoff, base+string(filepath.Separator)) {
				t.Fatalf("handoff pointer escaped the result dir: %q", out.Error.Handoff)
			}
		}
		for k, v := range readUsage(resultDir, log) {
			if v < 0 {
				t.Fatalf("negative usage %s=%d survived", k, v)
			}
		}
	})
}

// FuzzParseItems (review §5.5): parseItems never panics; every accepted item
// has a non-empty id; and validateItems rejects the reserved/escaping id
// grammar the instance-node ids and CEL rename depend on.
func FuzzParseItems(f *testing.F) {
	f.Add([]byte(`{"items":[{"id":"a"},{"id":"b","deps":["a"]}]}`))
	f.Add([]byte(`{"items":[{"id":"a]/b"}]}`))
	f.Add([]byte(`{"items":[{"id":""}]}`))
	f.Add([]byte(`{"items":[{"id":"a"},{"id":"a"}]}`))
	f.Add([]byte(`{"other":[]}`))
	f.Add([]byte(`{"items":[{"id":"a"}]} trailing`))
	f.Add([]byte(`{"items":[{"id":"a","deps":[1,2]}]}`))
	f.Add([]byte(`not json`))

	f.Fuzz(func(t *testing.T, data []byte) {
		items, err := parseItems(data) // never panics; id type-checked only
		if err != nil {
			return
		}
		// validateItems owns the grammar: whatever it accepts must be
		// non-empty, unique, and inside the closed id grammar.
		if verr := validateItems(items, map[string]config.BindingDesc{}); verr == nil {
			seen := map[string]bool{}
			for _, it := range items {
				if it.ID == "" || seen[it.ID] || !itemIDPattern.MatchString(it.ID) {
					t.Fatalf("validateItems accepted a bad id %q", it.ID)
				}
				seen[it.ID] = true
			}
		}
	})
}
