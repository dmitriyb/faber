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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sort"

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

// Executor satisfies the config CLI's Executor seam. Every field is an
// injected capability; nothing global. Store and (for real runs) Boxes are
// required; the rest degrade gracefully (nil Images hashes with an empty tag,
// nil Source fails generate nodes with a contract error, nil JSONOut skips
// the machine report).
type Executor struct {
	Store     *failure.Store        // run journals (required)
	Boxes     BoxRunner             // box-run seam (required to execute agent nodes)
	Hooks     failure.HookRunner    // on_failure cleanup
	Source    infra.CommandRunner   // generate data-source commands
	Workflows map[string]*config.IR // generate target IRs by workflow name
	Images    ImageTagger           // template image tags (journal hash input)
	Reentry   failure.BoxReentry    // interactive recovery seam
	Meta      RunMeta

	Out     io.Writer // human report; nil skips it
	JSONOut io.Writer // machine report; nil skips it
	Clock   Clock     // nil means the real clock
}

var _ config.Executor = (*Executor)(nil)

// Execute runs a validated IR to settlement and reports. Modes: "" and
// "fresh" begin a new journal (fresh always mints a new run id — --no-cache
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

	seed, err := e.openRun(ir, opts, clock)
	if err != nil {
		return err
	}
	defer seed.Journal.Close()
	runID := seed.Header.RunID
	runDir := e.Store.RunDir(runID)
	journalPath := filepath.Join(runDir, "journal.jsonl")
	log := logger.With("run", runID)

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

	// The report runs unconditionally — whatever reached the journal is
	// reportable, even when the run aborted mid-flight.
	report := e.render(runID, journalPath, ir, log)

	if runErr != nil {
		return runErr
	}
	if report != nil && report.Run.Totals.Failed > 0 {
		return fmt.Errorf("pipeline: run %s: %d step(s) failed", runID, report.Run.Totals.Failed)
	}
	return nil
}

// openRun seeds the scheduler per recovery mode: a fresh journal (with the
// caller's run id, or a minted one), a forced-fresh journal (--no-cache:
// always a new id, empty lookup), or the prior journal replayed for resume.
func (e *Executor) openRun(ir *config.IR, opts config.RunOptions, clock Clock) (*failure.RunSeed, error) {
	switch opts.Mode {
	case "resume":
		if opts.RunID == "" {
			return nil, errors.New("pipeline: resume requires a run id")
		}
		return e.Store.Resume(ir, opts.RunID, e.Meta.Supplied)
	case "", "fresh":
		runID := opts.RunID
		if runID == "" || opts.Mode == "fresh" {
			runID = failure.NewRunID()
		}
		irHash, err := config.HashIR(ir)
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
			Started:    clock.Now(),
		})
	default:
		return nil, fmt.Errorf("pipeline: unknown run mode %q", opts.Mode)
	}
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
