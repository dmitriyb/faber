package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
)

// RunReport is the settled run's summary — a pure function of the journal
// replay plus the IR, never of scheduler memory, so a crashed run, a killed
// run, and a freshly settled run all report through the same code path.
type RunReport struct {
	Run      RunHeader   `json:"run"`
	Steps    []StepLine  `json:"steps"`
	Generate []GenRollup `json:"generate,omitempty"`
}

// RunHeader is the report's run-level frame.
type RunHeader struct {
	RunID      string            `json:"run_id"`
	Workflow   string            `json:"workflow"`
	ConfigHash string            `json:"config_hash,omitempty"`
	IRHash     string            `json:"ir_hash,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	Totals     Totals            `json:"totals"`
	Wall       string            `json:"wall,omitempty"`
	Journal    string            `json:"journal,omitempty"`
}

// Totals counts terminal states across the whole run, generate instances
// included.
type Totals struct {
	OK                int `json:"ok"`
	Failed            int `json:"failed"`
	SkippedCondition  int `json:"skipped_condition"`
	SkippedDependency int `json:"skipped_dependency"`
	Absent            int `json:"absent"`
}

// StepLine is one node's report line.
type StepLine struct {
	ID          string               `json:"id"`
	Status      string               `json:"status"` // ok|failed|skipped-condition|skipped-dependency|absent
	Cached      bool                 `json:"cached,omitempty"`
	Duration    string               `json:"duration,omitempty"`
	Attempts    int                  `json:"attempts,omitempty"`
	Deferred    int                  `json:"deferred,omitempty"`
	DeferredFor string               `json:"deferred_for,omitempty"`
	Outputs     map[string]any       `json:"outputs,omitempty"`
	Error       *failure.ErrorRecord `json:"error,omitempty"`
	Ancestor    string               `json:"ancestor,omitempty"` // skipped-dependency root cause
	Chose       string               `json:"chose,omitempty"`    // selector's resolved candidate
}

// GenRollup groups one generate node's fan-out.
type GenRollup struct {
	Node    string    `json:"node"`
	Summary string    `json:"summary"`
	Items   []GenItem `json:"items"`
}

// GenItem is one instance's rollup: aggregate status plus its own step lines.
type GenItem struct {
	ID       string     `json:"id"`
	Status   string     `json:"status"`
	Duration string     `json:"duration,omitempty"`
	OK       int        `json:"ok"`
	Failed   int        `json:"failed"`
	Skipped  int        `json:"skipped"`
	Steps    []StepLine `json:"steps"`
}

// RunReporter derives run reports from journal replays.
type RunReporter struct{}

// irNodeInfo is the reporter's join key over the IR: kind and selector shape
// per node id, sub-workflows flattened.
type irNodeInfo struct {
	kind string
	sel  *config.SelSpec
}

func collectIRNodes(ir *config.IR, out map[string]irNodeInfo) {
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		out[n.ID] = irNodeInfo{kind: n.Kind, sel: n.Sel}
		if n.Sub != nil {
			collectIRNodes(n.Sub, out)
		}
	}
}

// Report joins the journal replay against the IR: every IR node gets a line
// (absent when no record exists — the report never silently shrinks), and
// journal records under a generate-instance prefix fold into per-item rollups
// beneath their generate node. Ordering is the IR's canonical node order —
// sorted path-like ids — so two runs of the same config order identically.
func (RunReporter) Report(rp *failure.Replay, ir *config.IR, journalPath string) (*RunReport, error) {
	if rp == nil {
		return nil, fmt.Errorf("pipeline: report: nil journal replay")
	}
	irNodes := map[string]irNodeInfo{}
	if ir != nil {
		collectIRNodes(ir, irNodes)
	}
	var genIDs []string
	for id, info := range irNodes {
		if info.kind == config.KindGenerate {
			genIDs = append(genIDs, id)
		}
	}
	sort.Strings(genIDs)

	// Partition journal records: IR-resident nodes vs generate-instance nodes
	// (grouped by generate node and item id) vs strays (defensive).
	type instKey struct{ gen, item string }
	instRecords := map[instKey]map[string]failure.ResultRecord{}
	topRecords := map[string]failure.ResultRecord{}
	for id, rec := range rp.LastByStep {
		if _, ok := irNodes[id]; ok {
			topRecords[id] = rec
			continue
		}
		if gen, item, ok := instanceOf(id, genIDs); ok {
			k := instKey{gen, item}
			if instRecords[k] == nil {
				instRecords[k] = map[string]failure.ResultRecord{}
			}
			instRecords[k][id] = rec
			continue
		}
		topRecords[id] = rec // stray record: still reported, never dropped
	}

	report := &RunReport{
		Run: RunHeader{
			RunID:      rp.Header.RunID,
			Workflow:   rp.Header.Workflow,
			ConfigHash: rp.Header.ConfigHash,
			IRHash:     rp.Header.IRHash,
			Params:     rp.Header.Params,
			Journal:    journalPath,
		},
	}

	// Top-level lines: the IR's node universe plus any stray journaled ids.
	universe := map[string]bool{}
	for id := range irNodes {
		universe[id] = true
	}
	for id := range topRecords {
		universe[id] = true
	}
	for _, id := range sortedKeys(universe) {
		rec, ok := topRecords[id]
		var line StepLine
		if !ok {
			line = StepLine{ID: id, Status: StateAbsent}
		} else {
			line = lineFromRecord(id, rec)
		}
		if info, ok := irNodes[id]; ok && info.kind == config.KindSelector && line.Status == StateOK {
			line.Chose = chooseCandidate(info.sel, rp)
		}
		report.tally(line.Status)
		report.Steps = append(report.Steps, line)
	}

	// Generate rollups, one per generate node in id order, items in id order.
	for _, gen := range genIDs {
		items := map[string]map[string]failure.ResultRecord{}
		for k, recs := range instRecords {
			if k.gen == gen {
				items[k.item] = recs
			}
		}
		rollup := GenRollup{Node: gen}
		for _, itemID := range sortedKeys(items) {
			gi := GenItem{ID: itemID}
			depSkipped := 0
			var start, finish time.Time
			for _, stepID := range sortedKeys(items[itemID]) {
				line := lineFromRecord(stepID, items[itemID][stepID])
				gi.Steps = append(gi.Steps, line)
				report.tally(line.Status)
				switch line.Status {
				case StateOK:
					gi.OK++
				case StateFailed:
					gi.Failed++
				case StateSkippedDependency:
					gi.Skipped++
					depSkipped++
				default:
					gi.Skipped++
				}
				rec := items[itemID][stepID]
				s, f := recordSpan(rec.Result)
				if !s.IsZero() && (start.IsZero() || s.Before(start)) {
					start = s
				}
				if f.After(finish) {
					finish = f
				}
			}
			// Condition skips are the workflow working as declared; only a
			// failure or a dependency skip degrades the item's aggregate.
			switch {
			case gi.Failed > 0:
				gi.Status = StateFailed
			case depSkipped > 0:
				gi.Status = StateSkippedDependency
			default:
				gi.Status = StateOK
			}
			if !start.IsZero() && finish.After(start) {
				gi.Duration = finish.Sub(start).String()
			}
			rollup.Items = append(rollup.Items, gi)
		}
		rollup.Summary = genSummary(rollup.Items)
		report.Generate = append(report.Generate, rollup)
	}

	// Wall clock: journal start to the newest record finish.
	if !rp.Header.Started.IsZero() {
		var last time.Time
		for _, rec := range rp.LastByStep {
			if _, f := recordSpan(rec.Result); f.After(last) {
				last = f
			}
		}
		if last.After(rp.Header.Started) {
			report.Run.Wall = last.Sub(rp.Header.Started).String()
		}
	}
	return report, nil
}

func (r *RunReport) tally(status string) {
	switch status {
	case StateOK:
		r.Run.Totals.OK++
	case StateFailed:
		r.Run.Totals.Failed++
	case StateSkippedCondition:
		r.Run.Totals.SkippedCondition++
	case StateSkippedDependency:
		r.Run.Totals.SkippedDependency++
	default:
		r.Run.Totals.Absent++
	}
}

// instanceOf matches a journal id against the generate-instance prefix
// convention "<gen-node-id>[<item-id>]/...".
func instanceOf(id string, genIDs []string) (gen, item string, ok bool) {
	for _, g := range genIDs {
		prefix := g + "["
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		rest := id[len(prefix):]
		end := strings.Index(rest, "]/")
		if end < 0 {
			continue
		}
		return g, rest[:end], true
	}
	return "", "", false
}

// lineFromRecord decodes one journal record into its report line: the skip
// encodings map back to their terminal states, annotations become the cached
// flag and defer counts, and real attempt history sets the attempt count.
func lineFromRecord(id string, rec failure.ResultRecord) StepLine {
	line := StepLine{ID: id}
	res := rec.Result
	deferred, deferredFor, cached, _ := decodeAnnotations(res.Attempts)
	line.Cached = cached
	line.Deferred = deferred
	if deferredFor > 0 {
		line.DeferredFor = deferredFor.String()
	}
	if s, f := recordSpan(res); !s.IsZero() && f.After(s) {
		line.Duration = f.Sub(s).String()
	}
	switch {
	case res.Status == failure.StatusOK:
		line.Status = StateOK
		line.Attempts = res.Attempt
		var outputs map[string]any
		if err := json.Unmarshal(res.Payload, &outputs); err == nil && len(outputs) > 0 {
			line.Outputs = outputs
		}
	case isSkipRecord(rec, reasonSkippedCondition):
		line.Status = StateSkippedCondition
		line.Duration = ""
	case isSkipRecord(rec, reasonSkippedDependency):
		line.Status = StateSkippedDependency
		line.Ancestor = res.Error.Detail
		line.Duration = ""
	default:
		line.Status = StateFailed
		line.Attempts = res.Attempt
		line.Error = res.Error
	}
	return line
}

// isSkipRecord reports whether a failed-status record is one of the
// scheduler's skip encodings: the reserved reason AND the null input hash
// (settleSkip's signature — genuine skips are always hashless, an executed
// failure always carries a real hash). Trusting the reason alone would let a
// box-authored record claiming "skipped-condition" zero the failure totals
// and flip the run's exit code.
func isSkipRecord(rec failure.ResultRecord, reason string) bool {
	return rec.InputHash == "" && rec.Result.Error != nil && rec.Result.Error.Reason == reason
}

// isAnnotation reports whether one attempt-history entry is a
// scheduler-authored annotation (a defer occurrence or a cached adoption)
// rather than a real attempt. Annotations are unambiguous by construction:
// the scheduler writes them with Attempt == 0, while the failure policy's
// real history entries are always 1-based — so a box-authored failure reason
// that merely collides with the annotation vocabulary stays a real attempt.
func isAnnotation(a failure.AttemptInfo) bool {
	return a.Attempt == 0 && a.Error != nil &&
		(a.Error.Reason == reasonDeferred || a.Error.Reason == reasonCached)
}

// decodeAnnotations splits a record's attempt history into annotations
// (defers, cached adoption) and real attempts.
func decodeAnnotations(history []failure.AttemptInfo) (deferred int, deferredFor time.Duration, cached bool, real int) {
	for _, a := range history {
		if !isAnnotation(a) {
			real++
			continue
		}
		switch a.Error.Reason {
		case reasonDeferred:
			deferred++
			deferredFor += a.Timing.Duration()
		case reasonCached:
			cached = true
		}
	}
	return deferred, deferredFor, cached, real
}

// recordSpan is a record's wall-clock span: the first real attempt's start to
// the final attempt's finish. Annotation entries never extend the span.
func recordSpan(res failure.Result) (start, finish time.Time) {
	start, finish = res.Timing.Started, res.Timing.Finished
	for _, a := range res.Attempts {
		if isAnnotation(a) {
			continue
		}
		if !a.Timing.Started.IsZero() && (start.IsZero() || a.Timing.Started.Before(start)) {
			start = a.Timing.Started
		}
	}
	return start, finish
}

// chooseCandidate re-derives which candidate an ok selector adopted: the
// newest candidate with an ok record — the same rule the scheduler applied,
// recomputed from journal plus IR.
func chooseCandidate(sel *config.SelSpec, rp *failure.Replay) string {
	if sel == nil {
		return ""
	}
	for _, c := range sel.Candidates {
		if rec, ok := rp.LastByStep[c]; ok && rec.Result.Status == failure.StatusOK {
			return c
		}
	}
	return ""
}

func genSummary(items []GenItem) string {
	ok, failed, skipped := 0, 0, 0
	for _, it := range items {
		switch it.Status {
		case StateOK:
			ok++
		case StateFailed:
			failed++
		default:
			skipped++
		}
	}
	parts := []string{fmt.Sprintf("%d ok", ok)}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	return fmt.Sprintf("%d items: %s", len(items), strings.Join(parts, ", "))
}

// Text renders the human report: stable config order, one line per step,
// generate rollups with nested instance lines, failure blocks with the
// handoff pointer and the re-entry command, and a run footer.
func (r *RunReport) Text(w io.Writer) error {
	p := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...)
	}
	p("run %s  workflow %s", r.Run.RunID, r.Run.Workflow)
	for _, line := range r.Steps {
		p("  %s", stepText(line))
	}
	for _, g := range r.Generate {
		p("  generate %s: %s", g.Node, g.Summary)
		for _, it := range g.Items {
			p("    [%s] %s%s  %d ok / %d failed / %d skipped",
				it.ID, it.Status, textDuration(it.Duration), it.OK, it.Failed, it.Skipped)
			for _, line := range it.Steps {
				p("      %s", stepText(line))
			}
		}
	}
	blocks := r.failureBlocks()
	if len(blocks) > 0 {
		p("failures:")
		for _, b := range blocks {
			for _, l := range b {
				p("  %s", l)
			}
		}
	}
	t := r.Run.Totals
	footer := fmt.Sprintf("totals: %d ok, %d failed, %d skipped (condition), %d skipped (dependency)",
		t.OK, t.Failed, t.SkippedCondition, t.SkippedDependency)
	if t.Absent > 0 {
		footer += fmt.Sprintf(", %d absent", t.Absent)
	}
	if r.Run.Wall != "" {
		footer += "; wall " + r.Run.Wall
	}
	p("%s", footer)
	if r.Run.Journal != "" {
		p("journal: %s", r.Run.Journal)
	}
	return nil
}

func stepText(line StepLine) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-19s %s", line.Status, line.ID)
	if line.Cached {
		sb.WriteString("  (cached)")
	}
	if line.Duration != "" {
		fmt.Fprintf(&sb, "  %s", line.Duration)
	}
	if line.Attempts > 1 {
		fmt.Fprintf(&sb, "  attempts=%d", line.Attempts)
	}
	if line.Deferred > 0 {
		fmt.Fprintf(&sb, "  deferred=%d wait=%s", line.Deferred, line.DeferredFor)
	}
	if line.Chose != "" {
		fmt.Fprintf(&sb, "  chose=%s", line.Chose)
	}
	if line.Ancestor != "" {
		fmt.Fprintf(&sb, "  after-failure-of=%s", line.Ancestor)
	}
	for _, k := range sortedKeys(line.Outputs) {
		fmt.Fprintf(&sb, "  %s=%v", k, line.Outputs[k])
	}
	return sb.String()
}

func textDuration(d string) string {
	if d == "" {
		return ""
	}
	return "  " + d
}

// failureBlocks builds the per-failure diagnostic blocks across top-level and
// instance lines, in report order.
func (r *RunReport) failureBlocks() [][]string {
	var blocks [][]string
	add := func(line StepLine) {
		if line.Status != StateFailed || line.Error == nil {
			return
		}
		b := []string{fmt.Sprintf("%s: %s: %s", line.ID, line.Error.Reason, line.Error.Detail)}
		if line.Error.Handoff != "" {
			b = append(b, fmt.Sprintf("  handoff: %s", line.Error.Handoff))
		}
		if line.Attempts > 1 {
			b = append(b, fmt.Sprintf("  attempts: %d", line.Attempts))
		}
		b = append(b, fmt.Sprintf("  re-enter: faber resume %s --interactive %s", r.Run.RunID, line.ID))
		blocks = append(blocks, b)
	}
	for _, line := range r.Steps {
		add(line)
	}
	for _, g := range r.Generate {
		for _, it := range g.Items {
			for _, line := range it.Steps {
				add(line)
			}
		}
	}
	return blocks
}

// JSON renders the machine report: one stably ordered document (struct field
// order fixed, slices pre-sorted, map keys sorted by encoding/json).
func (r *RunReport) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return fmt.Errorf("pipeline: encode report: %w", err)
	}
	return nil
}
