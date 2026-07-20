// Package pipeline is the IR executor: topological parallel scheduling over
// the desugared DAG, CEL condition evaluation, run-time generate expansion
// over user data-source items, failure propagation with skip semantics, and
// journal-derived run reporting.
//
// The package is mechanism, not policy: it schedules a proven graph, runs
// opaque boxes behind a fakeable seam, and reports what the journal says
// happened. It never learns what a step means.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/infra"
	"github.com/dmitriyb/faber/metering"
)

// RunMeta is invocation metadata the config CLI's Executor seam does not
// carry: the journal header's config identity and the supplied (string-form)
// params. The cmd wiring fills it per invocation; resume derives it from the
// prior journal's header.
type RunMeta struct {
	ConfigPath string
	ConfigHash string
	Supplied   map[string]string
}

// ImageChecker is the slice of the docker adapter the run preflight needs:
// existence of a tag in the daemon (and, through the first call, daemon
// reachability). infra's DockerClient satisfies it.
type ImageChecker interface {
	ImageExists(ctx context.Context, tag string) (bool, error)
}

// Executor satisfies the config CLI's Executor seam. Every field is an
// injected capability; nothing global. Store and (for real runs) Boxes are
// required; the rest degrade gracefully (nil Images hashes with an empty tag,
// nil Source fails generate nodes with a contract error, nil JSONOut skips
// the machine report, nil ImageCheck skips the run preflight).
type Executor struct {
	Store      *failure.Store        // run journals (required)
	Boxes      BoxRunner             // box-run seam (required to execute agent nodes)
	Hooks      failure.HookRunner    // on_failure cleanup
	Source     infra.CommandRunner   // generate data-source commands
	Workflows  map[string]*config.IR // generate target IRs by workflow name
	Images     ImageTagger           // template image tags (journal hash input)
	ImageCheck ImageChecker          // run preflight: images present, daemon up
	Reentry    failure.BoxReentry    // interactive recovery seam
	Meta       RunMeta

	Out     io.Writer // human report; nil skips it
	JSONOut io.Writer // machine report; nil skips it
	Clock   Clock     // nil means the real clock
}

var _ config.Executor = (*Executor)(nil)

// Execute runs a validated IR to settlement and reports. Modes: "" and
// "fresh" begin a new journal (fresh always mints a new run id — --fresh
// semantics); "resume" replays the prior journal so settled steps become
// lookup hits; "interactive" reconstructs a failed step's box and schedules
// nothing. The returned error is non-nil for process-level aborts and for
// runs with failed steps (the CLI's nonzero exit); condition skips alone are
// success.
func (e *Executor) Execute(ctx context.Context, ir *config.IR, params config.Params, opts config.RunOptions, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if e.Store == nil {
		return errors.New("pipeline: no journal store is wired into this executor")
	}
	if ir == nil {
		return errors.New("pipeline: nil IR")
	}
	clock := e.Clock
	if clock == nil {
		clock = realClock{}
	}

	if opts.Mode == "interactive" {
		if e.Reentry == nil {
			return errors.New("pipeline: interactive recovery requires a box re-entry seam, which is not wired")
		}
		if opts.InteractiveStep == "" {
			return errors.New("pipeline: interactive recovery requires --interactive <step-id>")
		}
		return e.Store.Interactive(ctx, opts.RunID, opts.InteractiveStep, e.Reentry)
	}

	// Preflight before any run state is minted: a dead daemon or an unbuilt
	// image fails once, up front, instead of once per step with retries.
	if err := e.preflight(ctx, ir); err != nil {
		return err
	}

	seed, err := e.openRun(ir, opts, clock)
	if err != nil {
		return err
	}
	defer seed.Journal.Close()
	runID := seed.Header.RunID
	runDir := e.Store.RunDir(runID)
	journalPath := filepath.Join(runDir, "journal.jsonl")
	log := logger.With("run", runID)

	// Every execution that opened a journal ends it with a run-end marker —
	// including setup failures between here and the scheduler — so a
	// `--budget` typo does not mint a run that audits unfinished forever.
	ended := false
	endRun := func(rec failure.RunEndRecord) {
		ended = true
		rec.Finished = clock.Now()
		if err := seed.Journal.AppendRunEnd(rec); err != nil {
			log.Warn("append run-end marker", "err", err)
		}
	}
	defer func() {
		if !ended {
			endRun(failure.RunEndRecord{Status: failure.RunEndAborted, Detail: "setup failed before scheduling"})
		}
	}()

	budgets, err := metering.ParseBudgets(opts.Budgets)
	if err != nil {
		return err
	}
	var mcfg *metering.Config
	if opts.MeteringPath != "" {
		mcfg, err = metering.LoadConfig(opts.MeteringPath, templateNames(ir, e.Workflows))
		if err != nil {
			return err
		}
	}
	admit := metering.NewAdmitter(metering.BuildMeters(mcfg, nil, log), budgets, log)
	foldJournaledCosts(admit, seed, log)
	rld := metering.NewRateLimitDefer(metering.DeferPolicy{}, log)
	restoreDeferCounts(rld, seed)

	ce, err := newCondEval()
	if err != nil {
		return err
	}
	g := newGraph()
	rootEnv := &scopeEnv{params: paramValues(params)}
	if _, err := g.addLevel(ir, rootEnv, ce); err != nil {
		return err
	}

	s := &scheduler{
		g:       g,
		ce:      ce,
		scopes:  map[string]*scopeEnv{},
		maxPar:  opts.MaxParallel,
		events:  make(chan event),
		done:    make(chan struct{}),
		seed:    seed,
		journal: seed.Journal,
		policy:  &failure.Policy{Journal: seed.Journal, Hooks: e.Hooks, Log: log},
		admit:   admit,
		mcfg:    mcfg,
		rld:     rld,
		boxes:   e.Boxes,
		images:  e.Images,
		expand:  &expander{runner: e.Source, irs: e.Workflows},
		clock:   clock,
		log:     log,
		runID:   runID,
		runDir:  runDir,
	}
	s.registerScopes(ir)

	runErr := s.run(ctx)

	// The run-end marker is this execution's durable terminal state: its
	// absence is the signature of an interrupted run (what the pre-upgrade
	// guard looks for). Best-effort — an unappendable journal already
	// surfaced as the run error.
	end := failure.RunEndRecord{Status: failure.RunEndSettled, Failed: s.failed}
	if runErr != nil {
		end.Status = failure.RunEndAborted
		end.Detail = runErr.Error()
	}
	endRun(end)

	// The report runs unconditionally — whatever reached the journal is
	// reportable, even when the run aborted mid-flight.
	e.render(runID, journalPath, ir, log)

	if runErr != nil {
		return runErr
	}
	// Exit accounting comes from the scheduler's own counter, not from
	// re-reading the journal: a report-load failure must never turn a run
	// with failed steps into exit 0.
	if s.failed > 0 {
		return fmt.Errorf("pipeline: run %s: %d step(s) failed", runID, s.failed)
	}
	return nil
}

// openRun seeds the scheduler per recovery mode: a fresh journal (with the
// caller's run id, or a minted one), a forced-fresh journal (--fresh:
// always a new id, empty lookup), or the prior journal replayed for resume.
func (e *Executor) openRun(ir *config.IR, opts config.RunOptions, clock Clock) (*failure.RunSeed, error) {
	switch opts.Mode {
	case "resume":
		if opts.RunID == "" {
			return nil, errors.New("pipeline: resume requires a run id")
		}
		seed, err := e.Store.Resume(ir, opts.RunID, e.Meta.Supplied)
		if err != nil {
			return nil, err
		}
		if err := e.checkImageDrift(ir, seed.Header.Images); err != nil {
			seed.Journal.Close()
			return nil, fmt.Errorf("pipeline: resume %s: %w", opts.RunID, err)
		}
		return seed, nil
	case "", "fresh":
		runID := opts.RunID
		if runID == "" || opts.Mode == "fresh" {
			runID = failure.NewRunID()
		}
		irHash, err := config.HashIR(ir)
		if err != nil {
			return nil, err
		}
		images, err := e.resolveImages(ir)
		if err != nil {
			return nil, err
		}
		return e.Store.Fresh(failure.Header{
			RunID:      runID,
			ConfigPath: e.Meta.ConfigPath,
			ConfigHash: e.Meta.ConfigHash,
			Workflow:   ir.Workflow,
			Params:     e.Meta.Supplied,
			IRHash:     irHash,
			IRVersion:  ir.IRVersion,
			Images:     images,
			Started:    clock.Now(),
		})
	default:
		return nil, fmt.Errorf("pipeline: unknown run mode %q", opts.Mode)
	}
}

// resolveImages derives every reachable template's image tag for the journal
// header: the engine-compiled inputs (default pin, image schema) the IR hash
// cannot see, recorded so resume can detect their drift. nil Images (unit
// wiring) records nothing — the scheduler then hashes with empty tags too.
func (e *Executor) resolveImages(ir *config.IR) (map[string]string, error) {
	if e.Images == nil {
		return nil, nil
	}
	images := map[string]string{}
	for _, tpl := range collectTemplates(ir, e.Workflows) {
		tag, err := e.Images.Tag(tpl)
		if err != nil {
			return nil, fmt.Errorf("pipeline: resolve image tag for template %s: %w", tpl.Name, err)
		}
		images[tpl.Name] = tag
	}
	return images, nil
}

// checkImageDrift compares the journaled per-template tags against the
// current derivation. The IR hash already pins template-declared inputs
// (packages, overlay path, pin); this closes the engine-side gap — a bumped
// default pin or image schema changes every tag, silently invalidating every
// journal key while the IR guard passes. Fail closed instead: the operator
// finishes on the old faber or starts over with --fresh.
func (e *Executor) checkImageDrift(ir *config.IR, journaled map[string]string) error {
	if e.Images == nil || len(journaled) == 0 {
		return nil
	}
	current, err := e.resolveImages(ir)
	if err != nil {
		return err
	}
	var drifted []string
	for _, name := range sortedKeys(journaled) {
		cur, ok := current[name]
		switch {
		case !ok:
			// The IR hash covers entry-IR templates, but generate-target IRs
			// are outside it — a target-only template disappearing is drift
			// only this comparison can see.
			drifted = append(drifted, fmt.Sprintf("%s (journaled, absent from the current derivation)", name))
		case cur != journaled[name]:
			drifted = append(drifted, fmt.Sprintf("%s (journal %s, current %s)", name, journaled[name], cur))
		}
	}
	for _, name := range sortedKeys(current) {
		if _, ok := journaled[name]; !ok {
			drifted = append(drifted, fmt.Sprintf("%s (new since the run was journaled)", name))
		}
	}
	if len(drifted) > 0 {
		return fmt.Errorf(
			"image inputs changed for template(s) %s — an engine or pin upgrade moved the image derivation, so journal keys cannot be trusted; finish the run on the faber that wrote it, or start over with --fresh",
			strings.Join(drifted, "; "))
	}
	return nil
}

// render loads the journal back and emits the report. Reporting is best
// effort next to a run error: failures here are logged, never masking the
// run's own outcome.
func (e *Executor) render(runID, journalPath string, ir *config.IR, log *slog.Logger) *RunReport {
	rp, err := e.Store.Load(runID)
	if err != nil {
		log.Error("load journal for report", "err", err)
		return nil
	}
	report, err := (RunReporter{}).Report(rp, ir, journalPath)
	if err != nil {
		log.Error("derive report", "err", err)
		return nil
	}
	if e.Out != nil {
		if err := report.Text(e.Out); err != nil {
			log.Error("render report", "err", err)
		}
	}
	if e.JSONOut != nil {
		if err := report.JSON(e.JSONOut); err != nil {
			log.Error("render JSON report", "err", err)
		}
	}
	return report
}

// preflight fails fast before any run state exists: every agent template
// reachable from the run (entry IR plus generate targets) must resolve its
// image tag, and the docker daemon must already hold each image. One early,
// aggregated refusal replaces a cascade of per-step launch failures each
// burning its full retry budget against a dead daemon or an unbuilt image.
// nil ImageCheck or nil Images (unit wiring) skips the check.
func (e *Executor) preflight(ctx context.Context, ir *config.IR) error {
	if e.ImageCheck == nil || e.Images == nil {
		return nil
	}
	var errs []error
	var missing []string
	for _, tpl := range collectTemplates(ir, e.Workflows) {
		tag, err := e.Images.Tag(tpl)
		if err != nil {
			errs = append(errs, fmt.Errorf("pipeline: preflight: template %s: %w", tpl.Name, err))
			continue
		}
		ok, err := e.ImageCheck.ImageExists(ctx, tag)
		if err != nil {
			// The daemon itself is unreachable; one error covers every image.
			return fmt.Errorf("pipeline: preflight: docker daemon check: %w", err)
		}
		if !ok {
			missing = append(missing, fmt.Sprintf("%s (template %s)", tag, tpl.Name))
		}
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("pipeline: preflight: %d image(s) not in the docker daemon: %s — run `faber build` first",
			len(missing), strings.Join(missing, ", ")))
	}
	return errors.Join(errs...)
}

// collectTemplates gathers the distinct resolved templates reachable from the
// entry IR and the generate target IRs, in name order.
func collectTemplates(ir *config.IR, workflows map[string]*config.IR) []*config.ResolvedTemplate {
	seen := map[string]*config.ResolvedTemplate{}
	var walk func(*config.IR)
	walk = func(g *config.IR) {
		if g == nil {
			return
		}
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Template != nil && seen[n.Template.Name] == nil {
				seen[n.Template.Name] = n.Template
			}
			if n.Sub != nil {
				walk(n.Sub)
			}
		}
	}
	walk(ir)
	for _, name := range sortedKeys(workflows) {
		walk(workflows[name])
	}
	out := make([]*config.ResolvedTemplate, 0, len(seen))
	for _, name := range sortedKeys(seen) {
		out = append(out, seen[name])
	}
	return out
}

// foldJournaledCosts folds a resumed run's journaled cost records into the
// admitter's spent ledger, so a declared budget bounds the whole logical run
// rather than resetting at every interruption (arch_journal.md's promised
// fold). Undecodable cost bodies are logged and skipped — accounting is
// best-effort here exactly as at Settle.
func foldJournaledCosts(admit *metering.Admitter, seed *failure.RunSeed, log *slog.Logger) {
	var prior []metering.Cost
	for _, rec := range seed.Costs {
		var cc []metering.Cost
		if err := json.Unmarshal(rec.Cost, &cc); err != nil {
			log.Warn("journaled cost record undecodable; not folded into the budget ledger",
				"step", rec.StepID, "err", err)
			continue
		}
		prior = append(prior, cc...)
	}
	admit.FoldSpent(prior)
}

// templateNames collects every template name reachable from the entry IR and
// the generate target IRs — the metering config's coverage universe.
func templateNames(ir *config.IR, workflows map[string]*config.IR) []string {
	seen := map[string]bool{}
	var walk func(*config.IR)
	walk = func(g *config.IR) {
		if g == nil {
			return
		}
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Template != nil {
				seen[n.Template.Name] = true
			}
			if n.Sub != nil {
				walk(n.Sub)
			}
		}
	}
	walk(ir)
	for _, name := range sortedKeys(workflows) {
		walk(workflows[name])
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// restoreDeferCounts re-seeds the rate-limit defer floor's consecutive
// counters from the prior journal, so the Max bound survives a process
// restart. Only failed records count — a step that deferred and then settled
// ok had its counter reset — and only the trailing run of consecutive
// scheduler-authored defer annotations counts, mirroring the floor's own
// consecutive semantics; box-authored failure reasons never masquerade as
// annotations (the Attempt == 0 marker).
func restoreDeferCounts(rld *metering.RateLimitDefer, seed *failure.RunSeed) {
	counts := map[string]int{}
	for key, rec := range seed.Prior {
		if rec.Result.Status == failure.StatusOK {
			continue
		}
		n := 0
		atts := rec.Result.Attempts
		for i := len(atts) - 1; i >= 0; i-- {
			if isAnnotation(atts[i]) && atts[i].Error.Reason == reasonDeferred {
				n++
				continue
			}
			break
		}
		if n > counts[key.StepID] {
			counts[key.StepID] = n
		}
	}
	for step, n := range counts {
		rld.Restore(step, n)
	}
}
