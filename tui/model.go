package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/pipeline"
)

// ParamInfo is one workflow parameter as Home lists it.
type ParamInfo struct {
	Name, Type string
	Required   bool
	Default    string
}

// WorkflowInfo is one workflow of the project with its parameters.
type WorkflowInfo struct {
	Name   string
	Params []ParamInfo
}

// TemplateInfo is one template and whether its image is in the daemon.
type TemplateInfo struct {
	Name    string
	Tag     string
	Built   bool
	Checked bool // false when no probe is wired or the daemon was unreachable
	Err     string
}

// Project is the config in the current directory as Home shows it: the
// validate result, the workflows, and the preflight's image states.
type Project struct {
	Path      string
	Err       error // load or validate failure; nothing else is shown then
	Workflows []WorkflowInfo
	Templates []TemplateInfo
	cfg       *config.Config
	irs       map[string]*config.IR // every workflow desugared, for the reporter's targets
}

// loadProject runs the same pipeline `faber validate` runs and asks the
// image probe about every template.
func loadProject(ctx context.Context, path string, probe ImageProbe) *Project {
	p := &Project{Path: path}
	cfg, viols, err := config.Load(path)
	if err != nil {
		p.Err = err
		return p
	}
	if err := config.Validate(cfg, viols); err != nil {
		p.Err = err
		return p
	}
	p.cfg = cfg
	p.irs = map[string]*config.IR{}
	for _, name := range sortedKeys(cfg.Workflows) {
		wf := cfg.Workflows[name]
		info := WorkflowInfo{Name: name}
		for _, pn := range sortedKeys(wf.Params) {
			def := wf.Params[pn]
			pi := ParamInfo{Name: pn, Type: def.Type, Required: def.Required}
			if def.Default != nil {
				pi.Default = fmt.Sprintf("%v", def.Default)
			}
			info.Params = append(info.Params, pi)
		}
		p.Workflows = append(p.Workflows, info)
		if ir, err := config.Desugar(cfg, name); err == nil {
			p.irs[name] = ir
		}
	}
	for _, name := range sortedKeys(cfg.Templates) {
		ti := TemplateInfo{Name: name}
		if probe != nil {
			var tpl *config.ResolvedTemplate
			for _, ir := range p.irs {
				for i := range ir.Nodes {
					if t := ir.Nodes[i].Template; t != nil && t.Name == name {
						tpl = t
					}
				}
			}
			if tpl != nil {
				if tag, err := probe.Tag(tpl); err != nil {
					ti.Err = err.Error()
				} else {
					ti.Tag = tag
					if ok, err := probe.Exists(ctx, tag); err != nil {
						ti.Err = err.Error()
					} else {
						ti.Checked, ti.Built = true, ok
					}
				}
			}
		}
		p.Templates = append(p.Templates, ti)
	}
	return p
}

// RunRow is one run of the store as Home lists it: the `faber runs` facts
// plus wall time and, for a live run, its current step.
type RunRow struct {
	ID, Name, Workflow, State string
	ConfigPath                string
	Started                   time.Time
	Wall                      time.Duration
	Now                       string // live: the crumb of the newest running step
	Failed                    int
	bounds                    map[string]int // journal-observed loop caps, for the crumb
}

// runState is the `faber runs` state vocabulary, re-derived from the same
// audit facts (live wins, then incomplete, then the run-end status).
func runState(a config.RunAudit) string {
	switch {
	case a.Live:
		return "LIVE"
	case !a.Complete:
		return "incomplete"
	case a.EndStatus != "":
		return a.EndStatus
	default:
		return "settled"
	}
}

// loadRuns lists every run, newest first, with the replay-derived facts.
func loadRuns(store *failure.Store, now time.Time) []RunRow {
	audits, err := store.AuditRuns()
	if err != nil {
		return nil
	}
	var rows []RunRow
	for _, a := range audits {
		row := RunRow{ID: a.RunID, Name: a.Name, Workflow: a.Workflow, State: runState(a), Started: a.Started}
		if hdr, err := store.LoadHeader(a.RunID); err == nil {
			row.ConfigPath = hdr.ConfigPath
		}
		if rp, err := store.Load(a.RunID); err == nil {
			row.Wall = wallOf(rp, a.Live, now)
			if rp.End != nil {
				row.Failed = rp.End.Failed
				if rp.End.Failed > 0 && row.State == failure.RunEndSettled {
					row.State = "FAILED"
				} else if row.State == failure.RunEndSettled {
					row.State = "ok"
				}
			}
			if a.Live {
				if r := running(rp, now); len(r) > 0 {
					row.Now = r[0].Step
					row.bounds = map[string]int{}
					for id := range rp.LastByStep {
						config.NoteLoopBounds(row.bounds, id)
					}
					for _, s := range rp.Starts {
						config.NoteLoopBounds(row.bounds, s.StepID)
					}
				}
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Started.After(rows[j].Started) })
	return rows
}

// wallOf is the run's wall clock: start to the newest record (or to now
// while live).
func wallOf(rp *failure.Replay, live bool, now time.Time) time.Duration {
	if rp.Header.Started.IsZero() {
		return 0
	}
	if live {
		return now.Sub(rp.Header.Started)
	}
	last := rp.Header.Started
	for _, rec := range rp.LastByStep {
		if f := rec.Result.Timing.Finished; f.After(last) {
			last = f
		}
	}
	for _, s := range rp.Starts {
		if s.Started.After(last) {
			last = s.Started
		}
	}
	if rp.End != nil && rp.End.Finished.After(last) {
		last = rp.End.Finished
	}
	return last.Sub(rp.Header.Started)
}

// Running is one step whose last start has no later result: running, or
// dead mid-box if the run is not live.
type Running struct {
	Step    string
	Attempt int
	Since   time.Time
}

// running derives in-flight steps from the journal alone: the newest start
// per step with no result finished after it.
func running(rp *failure.Replay, now time.Time) []Running {
	last := map[string]failure.StartRecord{}
	for _, s := range rp.Starts {
		last[s.StepID] = s
	}
	var out []Running
	for id, s := range last {
		if rec, ok := rp.LastByStep[id]; ok && !rec.Result.Timing.Finished.Before(s.Started) {
			continue
		}
		out = append(out, Running{Step: id, Attempt: s.Attempt, Since: s.Started})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out
}

// medians is the per-leaf-step-name median duration across every ok
// executed attempt in the store — the "usually 4m10s" next to a running
// step. Keyed by the id's last segment because that is what repeats across
// loops, instances, and runs.
func medians(store *failure.Store) map[string]time.Duration {
	audits, err := store.AuditRuns()
	if err != nil {
		return nil
	}
	samples := map[string][]time.Duration{}
	for _, a := range audits {
		rp, err := store.Load(a.RunID)
		if err != nil {
			continue
		}
		for key, rec := range rp.Results {
			if key.InputHash == "" || rec.Result.Status != failure.StatusOK {
				continue
			}
			d := rec.Result.Timing.Finished.Sub(rec.Result.Timing.Started)
			if d <= 0 {
				continue
			}
			leaf := key.StepID[strings.LastIndex(key.StepID, "/")+1:]
			samples[leaf] = append(samples[leaf], d)
		}
	}
	out := map[string]time.Duration{}
	for leaf, ds := range samples {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		out[leaf] = ds[len(ds)/2]
	}
	return out
}

// RunView is one run as the Run screen shows it.
type RunView struct {
	ID      string
	Replay  *failure.Replay
	Report  *pipeline.RunReport
	IR      *config.IR
	Live    bool
	Running []Running
	Bounds  map[string]int
	RunDir  string
	Medians map[string]time.Duration
	Err     error // the config could not be re-derived; the journal still shows
}

// loadRun replays the journal, re-derives the IR from the header's config
// (the way resume does), and reports; a config that no longer loads leaves
// the IR nil and the report journal-only.
func loadRun(store *failure.Store, id string, meds map[string]time.Duration, now time.Time) (*RunView, error) {
	rp, err := store.Load(id)
	if err != nil {
		return nil, err
	}
	v := &RunView{ID: id, Replay: rp, Live: store.RunLive(id), RunDir: store.RunDir(id), Medians: meds}
	var targets map[string]*config.IR
	if cfg, viols, err := config.Load(rp.Header.ConfigPath); err != nil {
		v.Err = err
	} else if err := config.Validate(cfg, viols); err != nil {
		v.Err = err
	} else {
		targets = map[string]*config.IR{}
		for name := range cfg.Workflows {
			if ir, err := config.Desugar(cfg, name); err == nil {
				targets[name] = ir
			}
		}
		v.IR = targets[rp.Header.Workflow]
	}
	report, err := (pipeline.RunReporter{Targets: targets}).Report(rp, v.IR, filepath.Join(v.RunDir, "journal.jsonl"))
	if err != nil {
		return nil, err
	}
	v.Report = report
	v.Running = running(rp, now)
	v.Bounds = config.LoopBounds(v.IR)
	for id := range rp.LastByStep {
		config.NoteLoopBounds(v.Bounds, id)
	}
	for _, s := range rp.Starts {
		config.NoteLoopBounds(v.Bounds, s.StepID)
	}
	return v, nil
}

// outputTail reads the last n lines of an attempt's output.log.
func outputTail(runDir, nodeID string, attempt, n int) []string {
	raw, err := os.ReadFile(filepath.Join(pipeline.AttemptDir(runDir, nodeID, attempt), pipeline.OutputLogName))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// lastAttempt is the attempt number of a step's newest start, or its
// result's attempt, or 1.
func lastAttempt(rp *failure.Replay, id string) int {
	n := 0
	for _, s := range rp.Starts {
		if s.StepID == id && s.Attempt > n {
			n = s.Attempt
		}
	}
	if n == 0 {
		if rec, ok := rp.LastByStep[id]; ok && rec.Result.Attempt > 0 {
			n = rec.Result.Attempt
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// crumb folds a node id into the breadcrumb the progress stream uses:
// root segment dropped, iterations as "name i/cap".
func crumb(id, workflow string, bounds map[string]int) string {
	if id == "" {
		return ""
	}
	segs := strings.Split(id, "/")
	first := 0
	if len(segs) > 1 && segs[0] == workflow {
		first = 1
	}
	parts := make([]string, 0, len(segs))
	for k := first; k < len(segs); k++ {
		if name, i, ok := config.IterationOf(segs[k]); ok {
			loop := strings.Join(append(append([]string{}, segs[:k]...), name), "/")
			if cap := bounds[loop]; cap > 0 {
				parts = append(parts, fmt.Sprintf("%s %d/%d", name, i, cap))
			} else {
				parts = append(parts, fmt.Sprintf("%s %d", name, i))
			}
			continue
		}
		parts = append(parts, segs[k])
	}
	return strings.Join(parts, " > ")
}
