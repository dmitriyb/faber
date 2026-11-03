package pipeline

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	// Loops folds the unrolled top-level loops back into iterations ran /
	// cap, additive beside Steps (which stays complete for machines).
	Loops []LoopRollup `json:"loops,omitempty"`

	// items is the human text's reading order: plain lines and loop
	// rollups interleaved in dependency order; sels maps selector ids to
	// their loops so an ancestor reads as the loop. Not serialized.
	items []ReadItem
	sels  map[string]string
	scope string
}

// ReadItem is one entry of the report's reading order: a plain step line or
// a loop rollup (exactly one is set). The text renderer and the TUI walk
// the same order.
type ReadItem struct {
	Line *StepLine
	Loop *LoopRollup
}

// RunHeader is the report's run-level frame.
type RunHeader struct {
	RunID      string            `json:"run_id"`
	Name       string            `json:"name,omitempty"`
	Workflow   string            `json:"workflow"`
	ConfigHash string            `json:"config_hash,omitempty"`
	IRHash     string            `json:"ir_hash,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	Totals     Totals            `json:"totals"`
	Wall       string            `json:"wall,omitempty"`
	Journal    string            `json:"journal,omitempty"`
	// Paused marks a run whose last run-end record says the cooperative
	// pause drain ended it — deliberately incomplete and resumable, not a
	// crash (undispatched steps still read absent).
	Paused bool `json:"paused,omitempty"`
}

// Totals counts terminal states across the whole run, generate instances
// included.
type Totals struct {
	OK                int `json:"ok"`
	Failed            int `json:"failed"`
	Halted            int `json:"halted,omitempty"`
	SkippedCondition  int `json:"skipped_condition"`
	SkippedDependency int `json:"skipped_dependency"`
	SkippedHalt       int `json:"skipped_halt,omitempty"`
	Absent            int `json:"absent"`
}

// StepLine is one node's report line.
type StepLine struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok|failed|halted|skipped-condition|skipped-dependency|skipped-halt|absent
	Cached bool   `json:"cached,omitempty"`
	// AgentSkipped marks a step whose attempt ran without an agent phase
	// (the prelude of an agent-skippable template made the decision).
	AgentSkipped bool                 `json:"agent_skipped,omitempty"`
	Duration     string               `json:"duration,omitempty"`
	Attempts     int                  `json:"attempts,omitempty"`
	Deferred     int                  `json:"deferred,omitempty"`
	DeferredFor  string               `json:"deferred_for,omitempty"`
	Outputs      map[string]any       `json:"outputs,omitempty"`
	Error        *failure.ErrorRecord `json:"error,omitempty"`
	Halt         *failure.HaltRecord  `json:"halt,omitempty"`     // halted step's reason/detail/phase
	Ancestor     string               `json:"ancestor,omitempty"` // skipped-dependency / skipped-halt root cause
	Chose        string               `json:"chose,omitempty"`    // selector's resolved candidate
}

// GenRollup groups one generate node's fan-out.
type GenRollup struct {
	Node    string    `json:"node"`
	Summary string    `json:"summary"`
	Items   []GenItem `json:"items"`
}

// GenItem is one instance's rollup: aggregate status plus its own step lines.
type GenItem struct {
	ID       string       `json:"id"`
	Status   string       `json:"status"`
	Duration string       `json:"duration,omitempty"`
	OK       int          `json:"ok"`
	Failed   int          `json:"failed"`
	Halted   int          `json:"halted,omitempty"`
	Skipped  int          `json:"skipped"`
	Steps    []StepLine   `json:"steps"`
	Loops    []LoopRollup `json:"loops,omitempty"`
	items    []ReadItem
}

// RunReporter derives run reports from journal replays. Targets, when set,
// supplies the generate target IRs (by workflow name) so instance lines read
// in the target's dependency order with its loop caps; without them an
// instance falls back to id order and journal-observed caps.
type RunReporter struct {
	Targets map[string]*config.IR
}

// irNodeInfo is the reporter's join key over the IR: kind and selector shape
// per node id, sub-workflows flattened.
type irNodeInfo struct {
	kind   string
	sel    *config.SelSpec
	target string // generate nodes: the fanned-out workflow name
}

func collectIRNodes(ir *config.IR, out map[string]irNodeInfo) {
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		info := irNodeInfo{kind: n.Kind, sel: n.Sel}
		if n.Gen != nil {
			info.target = n.Gen.Workflow
		}
		out[n.ID] = info
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
func (r RunReporter) Report(rp *failure.Replay, ir *config.IR, journalPath string) (*RunReport, error) {
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
			Name:       rp.Header.Name,
			Workflow:   rp.Header.Workflow,
			ConfigHash: rp.Header.ConfigHash,
			IRHash:     rp.Header.IRHash,
			Params:     rp.Header.Params,
			Journal:    journalPath,
			Paused:     rp.End != nil && rp.End.Status == failure.RunEndPaused,
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
	bounds := loopBounds(ir)
	sels := selectorLoops(irNodes)
	report.sels = sels
	// Top-level names read relative to the workflow prefix every IR id
	// carries; a stray id without it simply renders in full.
	report.scope = ""
	if ir != nil && ir.Workflow != "" {
		report.scope = ir.Workflow + "/"
	}
	report.items, report.Loops = orderAndRollup(report.Steps, topoOrder(ir, sortedKeys(universe)), report.scope, bounds, sels)

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
			depSkipped, haltSkipped := 0, 0
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
				case StateHalted:
					gi.Halted++
				case StateSkippedDependency:
					gi.Skipped++
					depSkipped++
				case StateSkippedHalt:
					gi.Skipped++
					haltSkipped++
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
			// failure, halt, or non-condition skip degrades the item's
			// aggregate. Failure outranks halt, mirroring the run exit; an
			// instance degraded only by an outside halt reads skipped-halt,
			// never as a failure cascade.
			switch {
			case gi.Failed > 0:
				gi.Status = StateFailed
			case gi.Halted > 0:
				gi.Status = StateHalted
			case haltSkipped > 0 && depSkipped == 0:
				gi.Status = StateSkippedHalt
			case depSkipped > 0:
				gi.Status = StateSkippedDependency
			default:
				gi.Status = StateOK
			}
			if !start.IsZero() && finish.After(start) {
				gi.Duration = finish.Sub(start).String()
			}
			// Instance ids are outside the entry IR: with the target IR at
			// hand they read in its dependency order under the instance
			// scope and take its loop caps; without it, id order and the
			// journal's own caps.
			scope := gen + "[" + itemID + "]/"
			ids := make([]string, 0, len(gi.Steps))
			for _, l := range gi.Steps {
				ids = append(ids, l.ID)
			}
			ibounds := bounds
			if tgt := r.Targets[irNodes[gen].target]; tgt != nil {
				ids = instanceOrder(tgt, scope, ids)
				ibounds = instanceBounds(tgt, scope, bounds)
			}
			isels := instanceSelectors(gi.Steps, rp, scope)
			for k, v := range isels {
				report.sels[k] = v
			}
			gi.items, gi.Loops = orderAndRollup(gi.Steps, ids, scope, ibounds, isels)
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

// orderAndRollup arranges lines in reading order and folds loops: plain
// lines keep their place, a loop takes the place of its first line.
func orderAndRollup(lines []StepLine, order []string, scope string, bounds map[string]int, sels map[string]string) ([]ReadItem, []LoopRollup) {
	byID := map[string]*StepLine{}
	for i := range lines {
		byID[lines[i].ID] = &lines[i]
	}
	ordered := make([]StepLine, 0, len(lines))
	for _, id := range order {
		if l, ok := byID[id]; ok {
			ordered = append(ordered, *l)
		}
	}
	plain, loops := rollupLoops(ordered, scope, bounds, sels)
	// Interleave: walk the ordered lines; emit a plain line where it sits,
	// and a loop where its first member sits.
	loopAt := map[string]int{} // first member id -> loop index
	placed := map[int]bool{}
	for i, lp := range loops {
		for _, l := range ordered {
			if lo, _, ok := loopOf(l.ID, scope); ok && lo == lp.Loop {
				loopAt[l.ID] = i
				break
			}
			if sels[l.ID] == lp.Loop {
				loopAt[l.ID] = i
				break
			}
		}
	}
	plainSet := map[string]bool{}
	for _, l := range plain {
		plainSet[l.ID] = true
	}
	var items []ReadItem
	for i := range ordered {
		l := &ordered[i]
		if plainSet[l.ID] {
			items = append(items, ReadItem{Line: l})
			continue
		}
		if li, ok := loopAt[l.ID]; ok && !placed[li] {
			placed[li] = true
			items = append(items, ReadItem{Loop: &loops[li]})
		}
	}
	return items, loops
}

// instanceSelectors finds a generate instance's selector lines by shape:
// an ok record whose payload the scheduler adopted from a candidate is
// indistinguishable here, so the selector is recognized the way the
// scheduler names it — a line whose id is a loop's prefix minus its last
// segment plus the body step, i.e. "<prefix>/<step>" with "<prefix>/<loop>@1/<step>"
// also present.
func instanceSelectors(lines []StepLine, rp *failure.Replay, scope string) map[string]string {
	ids := map[string]bool{}
	for _, l := range lines {
		ids[l.ID] = true
	}
	out := map[string]string{}
	for _, l := range lines {
		if _, _, isIter := loopOf(l.ID, scope); isIter {
			continue
		}
		rel := strings.TrimPrefix(l.ID, scope)
		step := rel[strings.LastIndex(rel, "/")+1:]
		parent := scope + strings.TrimSuffix(rel, step)
		for id := range ids {
			if !strings.HasPrefix(id, parent) || !strings.HasSuffix(id, "/"+step) {
				continue
			}
			if loop, i, ok := loopOf(id, parent); ok && i == 1 && id == loop+"@1/"+step {
				out[l.ID] = loop
				break
			}
		}
	}
	return out
}

// ReadingOrder is the top-level reading order (dependency order, loops
// folded) and Scope the prefix top-level names are relative to.
func (r *RunReport) ReadingOrder() []ReadItem { return r.items }
func (r *RunReport) Scope() string            { return r.scope }

// ReadingOrder is the instance's reading order under its own scope.
func (g *GenItem) ReadingOrder() []ReadItem { return g.items }

func (r *RunReport) tally(status string) {
	switch status {
	case StateOK:
		r.Run.Totals.OK++
	case StateFailed:
		r.Run.Totals.Failed++
	case StateHalted:
		r.Run.Totals.Halted++
	case StateSkippedCondition:
		r.Run.Totals.SkippedCondition++
	case StateSkippedDependency:
		r.Run.Totals.SkippedDependency++
	case StateSkippedHalt:
		r.Run.Totals.SkippedHalt++
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
	line.AgentSkipped = res.AgentSkipped
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
	case res.Status == failure.StatusHalted:
		line.Status = StateHalted
		line.Attempts = res.Attempt
		line.Halt = res.Halt
	case isSkipRecord(rec, reasonSkippedCondition):
		line.Status = StateSkippedCondition
		line.Duration = ""
	case isSkipRecord(rec, reasonSkippedDependency):
		line.Status = StateSkippedDependency
		line.Ancestor = res.Error.Detail
		line.Duration = ""
	case isSkipRecord(rec, reasonSkippedHalt):
		line.Status = StateSkippedHalt
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
	ok, failed, halted, notNeeded, notReached := 0, 0, 0, 0, 0
	for _, it := range items {
		switch it.Status {
		case StateOK:
			ok++
		case StateFailed:
			failed++
		case StateHalted:
			halted++
		case StateSkippedCondition:
			notNeeded++
		default:
			notReached++
		}
	}
	parts := []string{fmt.Sprintf("%d ok", ok)}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if halted > 0 {
		parts = append(parts, fmt.Sprintf("%d halted", halted))
	}
	if notNeeded > 0 {
		parts = append(parts, fmt.Sprintf("%d not needed", notNeeded))
	}
	if notReached > 0 {
		parts = append(parts, fmt.Sprintf("%d not reached", notReached))
	}
	return fmt.Sprintf("%d items: %s", len(items), strings.Join(parts, ", "))
}

// sanitizeTerm strips terminal control bytes from box-derived text before it
// reaches the operator's terminal: all C0 controls (newlines included — the
// report's lines are renderer-framed, and an embedded newline could spoof a
// whole report line), DEL, and the C1 range — an escape sequence in a
// payload value or an error detail must never re-style, retitle, clear, or
// forge the operator's screen. The JSON report path needs none of this
// (encoding escapes control bytes); this is for the human Text path only.
func sanitizeTerm(s string) string {
	// The fast path also requires valid UTF-8: a raw invalid byte (e.g. a bare
	// C1 introducer 0x9b) is not a control *rune*, so ContainsFunc would miss
	// it and return the bytes verbatim. The rebuild loop ranges over runes,
	// mapping every invalid byte to U+FFFD, so it is self-sufficient — it does
	// not lean on an upstream JSON round-trip to normalize the bytes first.
	if utf8.ValidString(s) && !strings.ContainsFunc(s, isTermControl) {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		if r == utf8.RuneError || isTermControl(r) {
			sb.WriteRune(utf8.RuneError)
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func isTermControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// failureBlocks builds the per-failure diagnostic blocks across top-level and
// instance lines, in report order.
func (r *RunReport) failureBlocks() [][]string {
	var blocks [][]string
	add := func(line StepLine) {
		if line.Status != StateFailed || line.Error == nil {
			return
		}
		b := []string{fmt.Sprintf("%s: %s: %s", line.ID, sanitizeTerm(line.Error.Reason), sanitizeTerm(line.Error.Detail))}
		if line.Error.Handoff != "" {
			b = append(b, fmt.Sprintf("  handoff: %s", sanitizeTerm(line.Error.Handoff)))
		}
		if line.Attempts > 1 {
			b = append(b, fmt.Sprintf("  attempts: %d", line.Attempts))
		}
		// The re-enter suggestion is offered only when there is handoff state
		// to reconstruct from — interactive mode refuses everything else, and
		// a suggestion that leads to a refusal is worse than none.
		if line.Error.Handoff != "" {
			b = append(b, fmt.Sprintf("  re-enter: faber resume %s --interactive %s", r.Run.RunID, line.ID))
		}
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

// haltBlocks builds the halted-step lines across top-level and instance
// lines, in report order: the halting step, its reason, its detail, and the
// phase that requested the stop — named without the reader parsing JSON.
// Halt fields are box-authored text and are terminal-sanitized like every
// other box-derived string on the Text path.
func (r *RunReport) haltBlocks() []string {
	var lines []string
	add := func(line StepLine) {
		if line.Status != StateHalted || line.Halt == nil {
			return
		}
		l := fmt.Sprintf("%s: %s", line.ID, sanitizeTerm(line.Halt.Reason))
		if line.Halt.Detail != "" {
			l += ": " + sanitizeTerm(line.Halt.Detail)
		}
		if line.Halt.Phase != "" {
			l += fmt.Sprintf("  (requested by the %s phase)", sanitizeTerm(line.Halt.Phase))
		}
		lines = append(lines, l)
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
	return lines
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
