package tui

import (
	"context"
	"errors"
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

// Check is one preflight fact about a configured thing: a role, a hook, an
// image, the credential resolver. State is the status vocabulary (ok,
// MISSING, unused, not checked, FAIL); Fix is the command that mends a
// MISSING; Detail is where the thing was looked for.
type Check struct {
	Name   string
	State  string
	Detail string
	Fix    string
	Used   bool // referenced by some template (an unused thing is never a problem)
}

// Preflight is what Home states about the config before anyone presses
// run: every declared role, every template's hook slots, the credential
// resolver and its services. Health is a count; a problem is a row.
type Preflight struct {
	Roles     []Check
	Templates int     // how many templates were checked
	Slots     []Check // only the hook slots whose script is not there: Name = template, Detail = slot, Fix = path
	Images    []Check // one per template the workflow runs (NEEDS only): ok, MISSING with faber build, FAIL, not checked
	Resolver  Check   // Name "" when no resolver is configured
	Services  []Check // state "not checked": only the resolver can answer
}

// Project is the config in the current directory as Home shows it: the
// validate result, the workflows, and the preflight.
type Project struct {
	Path      string
	Dir       string // the config's directory; paths on screen are relative to it
	Err       error  // load or validate failure; nothing else is shown then
	Workflows []WorkflowInfo
	Templates []TemplateInfo
	Preflight Preflight
	cfg       *config.Config
	irs       map[string]*config.IR // every workflow desugared, for the reporter's targets
	stats     History               // the store's per-step history, for the preview
	mtime     time.Time             // the config file's mtime this was derived from
}

// irCache keeps each config path's desugared workflows, re-derived when
// the file's mtime moves, so a run's IR is not re-validated per tick.
type irCache struct {
	entries map[string]*irEntry
}

type irEntry struct {
	mtime time.Time
	irs   map[string]*config.IR
	err   error
}

func newIRCache() *irCache { return &irCache{entries: map[string]*irEntry{}} }

// get returns every workflow of the config at path, desugared, or the
// load/validate error. A relative path is unknown: it was journaled as
// typed in some other working directory and must not be resolved here.
func (c *irCache) get(path string) (map[string]*config.IR, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("config path %q is relative in the journal; the run's project cannot be identified from here", path)
	}
	mt := configMTime(path)
	if e := c.entries[path]; e != nil && e.mtime.Equal(mt) {
		return e.irs, e.err
	}
	e := &irEntry{mtime: mt}
	cfg, viols, err := config.Load(path)
	if err != nil {
		e.err = err
	} else if err := config.Validate(cfg, viols); err != nil {
		e.err = err
	} else {
		e.irs = map[string]*config.IR{}
		for name := range cfg.Workflows {
			if ir, err := config.Desugar(cfg, name); err == nil {
				e.irs[name] = ir
			}
		}
	}
	c.entries[path] = e
	return e.irs, e.err
}

// configMTime is the config file's mtime, zero when it cannot be read:
// the key that decides whether a project or a run's IR is re-derived.
func configMTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// loadProject runs the same pipeline `faber validate` runs and asks the
// image probe about every template.
func loadProject(ctx context.Context, path string, probe ImageProbe, keys KeyProbe) *Project {
	p := &Project{Path: path, Dir: filepath.Dir(path), mtime: configMTime(path)}
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
	p.Preflight = preflight(ctx, cfg, p.irs, p.Dir, keys)
	return p
}

// templatesOf lists every agent template a workflow runs: its own nodes,
// its sub-workflows, and the workflows its generates expand into — the
// reference epic is a generate over task, so epic needs task's images.
func templatesOf(ir *config.IR, irs map[string]*config.IR) []*config.ResolvedTemplate {
	var out []*config.ResolvedTemplate
	visited := map[string]bool{}
	var walk func(*config.IR)
	walk = func(g *config.IR) {
		for i := range g.Nodes {
			n := &g.Nodes[i]
			if n.Template != nil {
				out = append(out, n.Template)
			}
			if n.Sub != nil {
				walk(n.Sub)
			}
			if n.Gen != nil && !visited[n.Gen.Workflow] {
				visited[n.Gen.Workflow] = true
				if target := irs[n.Gen.Workflow]; target != nil {
					walk(target)
				}
			}
		}
	}
	walk(ir)
	return out
}

// hookPaths lists a template's four hooks in their fixed order, "" when unset.
func hookPaths(t *config.ResolvedTemplate) [4][2]string {
	return [4][2]string{{"context", t.Hooks.Context}, {"prelude", t.Hooks.Prelude}, {"postlude", t.Hooks.Postlude}, {"on_failure", t.Hooks.OnFailure}}
}

// relPath shows a path the way the YAML spells it: an included file's
// paths come absolutised against their declaring directory and are shown
// relative to the config; the root file's are CWD-relative and pass
// through — which is also how a run opens them, so a stat on the value is
// the run's own test.
func relPath(dir, p string) string {
	if p == "" || !filepath.IsAbs(p) {
		return p
	}
	if r, err := filepath.Rel(dir, p); err == nil && !strings.HasPrefix(r, "..") {
		return "./" + r
	}
	return p
}

// preflight derives the checks: a role is a problem only when a template
// uses it and no key resolves (a path-form key by stat, a registry role
// through the probe); a template's hook slot only when its script is not
// there; the resolver only when configured and absent. Nothing is run.
func preflight(ctx context.Context, cfg *config.Config, irs map[string]*config.IR, dir string, keys KeyProbe) Preflight {
	var pf Preflight
	usedRoles := map[string]bool{}
	seen := map[string]bool{}
	for _, name := range sortedKeys(irs) {
		for _, t := range templatesOf(irs[name], irs) {
			if t.Identity != "" {
				usedRoles[t.Identity] = true
			}
			if seen[t.Name] {
				continue
			}
			seen[t.Name] = true
			pf.Templates++
			pf.Slots = append(pf.Slots, missingSlots(t, dir)...)
		}
	}
	sort.SliceStable(pf.Slots, func(i, j int) bool { return pf.Slots[i].Name < pf.Slots[j].Name })
	for _, role := range sortedKeys(cfg.Identities) {
		pf.Roles = append(pf.Roles, roleCheck(ctx, role, cfg.Identities[role], usedRoles[role], dir, keys))
	}
	if r := cfg.Credentials.Resolver; r != "" {
		pf.Resolver = Check{Name: filepath.Base(r), Detail: relPath(dir, r), State: "ok", Used: true}
		if _, err := os.Stat(r); err != nil {
			pf.Resolver.State = "MISSING"
		}
	}
	for _, name := range sortedKeys(cfg.Credentials.Services) {
		pf.Services = append(pf.Services, Check{Name: name, Detail: cfg.Credentials.Services[name].Mode, State: "not checked", Used: true})
	}
	return pf
}

// missingSlots lists a template's hook slots whose script is not where the
// template points, in slot order.
func missingSlots(t *config.ResolvedTemplate, dir string) []Check {
	var out []Check
	for _, h := range hookPaths(t) {
		if h[1] == "" {
			continue
		}
		if err := config.CheckHookFile(h[1]); err != nil {
			fix := relPath(dir, h[1])
			if !errors.Is(err, config.ErrHookNotFound) {
				fix += "   " + err.Error()
			}
			out = append(out, Check{Name: t.Name, Detail: h[0], Fix: fix, State: "MISSING", Used: true})
		}
	}
	return out
}

func roleCheck(ctx context.Context, role string, def config.IdentityDef, used bool, dir string, keys KeyProbe) Check {
	c := Check{Name: role, Used: used}
	if !used {
		c.State = "unused"
		return c
	}
	if def.Key != "" && !strings.HasPrefix(def.Key, "SHA256:") { // a path-form key: the run uses it verbatim
		c.Detail = relPath(dir, def.Key)
		c.State = "ok"
		if _, err := os.Stat(def.Key); err != nil {
			c.State = "MISSING"
			c.Fix = "key file not found"
		}
		return c
	}
	if keys == nil {
		c.State = "not checked"
		c.Detail = "registry"
		return c
	}
	src, err := keys.Resolve(ctx, role, def)
	if err != nil {
		c.State = "MISSING"
		c.Fix = "faber add-key --role " + role
		return c
	}
	c.State = "ok"
	c.Detail = "registry  " + src
	return c
}

// needsOf narrows the preflight to what one workflow runs: the roles its
// templates name and those templates' slots; the resolver and services
// are global.
func needsOf(p *Project, ir *config.IR) Preflight {
	roles := map[string]bool{}
	templates := map[string]bool{}
	for _, t := range templatesOf(ir, p.irs) {
		if t.Identity != "" {
			roles[t.Identity] = true
		}
		templates[t.Name] = true
	}
	var n Preflight
	for _, c := range p.Preflight.Roles {
		if roles[c.Name] {
			n.Roles = append(n.Roles, c)
		}
	}
	n.Templates = len(templates)
	for _, t := range p.Templates {
		if !templates[t.Name] {
			continue
		}
		c := Check{Name: t.Name, Used: true}
		switch {
		case t.Err != "":
			c.State, c.Detail = "FAIL", t.Err
		case t.Checked && t.Built:
			c.State = "ok"
		case t.Checked:
			c.State, c.Fix, c.Detail = "MISSING", "faber build", tagHash(t.Tag)
		default:
			c.State = "not checked"
		}
		n.Images = append(n.Images, c)
	}
	for _, c := range p.Preflight.Slots {
		if templates[c.Name] {
			n.Slots = append(n.Slots, c)
		}
	}
	n.Resolver, n.Services = p.Preflight.Resolver, p.Preflight.Services
	return n
}

// displayPath is the config path as the headers show it: relative to the
// working directory when it is near, so the same project reads the same on
// any machine; absolute otherwise.
func displayPath(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(r, "../..") {
			return r
		}
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

// runState is the `faber runs` state word, from the CLI's own derivation,
// with the two refinements the view adds: live is a badge (LIVE), and a
// settled run reads ok or FAILED by its failed count.
func runState(a config.RunAudit, rp *failure.Replay) string {
	state := config.RunState(a)
	switch {
	case state == "live":
		return "LIVE"
	case state == failure.RunEndSettled && rp != nil && rp.End != nil && rp.End.Failed > 0:
		return "FAILED"
	case state == failure.RunEndSettled:
		return "ok"
	}
	return state
}

// replayCache keeps one replay per run and re-reads a journal only when
// its size or mtime moved: the one-hertz tick then costs a stat per run,
// not a replay per run. version counts changes, so dependents (the
// store-wide history) recompute only when something new was journaled.
type replayCache struct {
	store   *failure.Store
	entries map[string]*cacheEntry
	version int
}

type cacheEntry struct {
	size  int64
	mtime time.Time
	rp    *failure.Replay
}

func newReplayCache(store *failure.Store) *replayCache {
	return &replayCache{store: store, entries: map[string]*cacheEntry{}}
}

// get returns the run's replay, re-reading it only when the journal
// changed on disk.
func (c *replayCache) get(id string) (*failure.Replay, error) {
	path := filepath.Join(c.store.RunDir(id), "journal.jsonl")
	st, err := os.Stat(path)
	if err != nil {
		delete(c.entries, id)
		return nil, err
	}
	if e := c.entries[id]; e != nil && e.size == st.Size() && e.mtime.Equal(st.ModTime()) {
		return e.rp, nil
	}
	rp, err := c.store.Load(id)
	if err != nil {
		return nil, err
	}
	c.entries[id] = &cacheEntry{size: st.Size(), mtime: st.ModTime(), rp: rp}
	c.version++
	return rp, nil
}

// loadRuns lists every run, newest first, with the replay-derived facts.
// A store that cannot be listed is an error the screen shows, never an
// empty table.
func loadRuns(cache *replayCache, now time.Time) ([]RunRow, error) {
	audits, err := cache.store.AuditRuns()
	if err != nil {
		return nil, err
	}
	var rows []RunRow
	for _, a := range audits {
		rp, rerr := cache.get(a.RunID)
		row := RunRow{ID: a.RunID, Name: a.Name, Workflow: a.Workflow, State: runState(a, rp), Started: a.Started}
		if rerr != nil {
			row.State = "unreadable"
			rows = append(rows, row)
			continue
		}
		row.ConfigPath = rp.Header.ConfigPath
		row.Wall = wallOf(rp, a.Live, now)
		if rp.End != nil {
			row.Failed = rp.End.Failed
		}
		if a.Live {
			if r := rp.Running(); len(r) > 0 {
				row.Now = r[0].StepID
				row.bounds = map[string]int{}
				for id := range rp.LastByStep {
					config.NoteLoopBounds(row.bounds, id)
				}
				for _, s := range rp.Starts {
					config.NoteLoopBounds(row.bounds, s.StepID)
				}
			}
		}
		rows = append(rows, row)
	}
	// live runs first whatever their age — they are what the operator acts
	// on — then newest first
	sort.SliceStable(rows, func(i, j int) bool {
		li, lj := rows[i].State == "LIVE", rows[j].State == "LIVE"
		if li != lj {
			return li
		}
		return rows[i].Started.After(rows[j].Started)
	})
	return rows, nil
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

// Running is the view's name for failure.RunningStep: a step whose last
// start has no later result — running, or dead mid-box if the run is not
// live. The derivation lives on the replay, shared with every reader.
type Running struct {
	Step    string
	Attempt int
	Since   time.Time
}

func runningOf(rp *failure.Replay) []Running {
	var out []Running
	for _, r := range rp.Running() {
		out = append(out, Running{Step: r.StepID, Attempt: r.Attempt, Since: r.Since})
	}
	return out
}

// Stats is a step name's duration distribution across ok executed
// attempts: p50 is the "usually" beside a running step, p90 and p99 say
// how bad it gets, n how much history backs them.
type Stats struct {
	P50, P90, P99 time.Duration
	N             int
}

// History is the store's distributions by workflow, then by step name (an
// id's last segment, which repeats across loops, instances and runs). A
// step is the same step only within its workflow: "review" in task and
// "review" in another workflow never share a bucket.
type History map[string]map[string]Stats

// stats computes the history over every replay the cache holds.
// Recomputed when the cache's version moves (a journal grew), never per
// tick.
func stats(cache *replayCache) History {
	samples := map[string]map[string][]time.Duration{}
	for _, e := range cache.entries {
		collect(samples, e.rp)
	}
	out := History{}
	for wf, byLeaf := range samples {
		out[wf] = distributions(byLeaf)
	}
	return out
}

// runStats is one run's own history: the same distributions over this
// replay alone, for the "this run" scope.
func runStats(rp *failure.Replay) map[string]Stats {
	samples := map[string]map[string][]time.Duration{}
	collect(samples, rp)
	return distributions(samples[rp.Header.Workflow])
}

func collect(samples map[string]map[string][]time.Duration, rp *failure.Replay) {
	wf := rp.Header.Workflow
	if samples[wf] == nil {
		samples[wf] = map[string][]time.Duration{}
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
		samples[wf][leaf] = append(samples[wf][leaf], d)
	}
}

func distributions(byLeaf map[string][]time.Duration) map[string]Stats {
	out := map[string]Stats{}
	for leaf, ds := range byLeaf {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		pct := func(p float64) time.Duration { return ds[int(float64(len(ds)-1)*p)] }
		out[leaf] = Stats{P50: pct(0.5), P90: pct(0.9), P99: pct(0.99), N: len(ds)}
	}
	return out
}

// HistoryScope names which attempts a view's Stats cover.
const (
	ScopeAllRuns = "all runs" // every run of this workflow in the store
	ScopeThisRun = "this run" // this run's own attempts
)

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
	Stats   map[string]Stats // the history in force, by step name
	Scope   string           // ScopeAllRuns or ScopeThisRun: what Stats cover, said on screen
	Err     error            // the config could not be re-derived; the journal still shows
}

// loadRun replays the journal (through the cache), re-derives the IR from
// the header's config the way resume does (through the IR cache), and
// reports; a config that no longer loads, or a relative journaled path,
// leaves the IR nil and the report journal-only with the reason on Err.
func loadRun(cache *replayCache, irs *irCache, id string, h History, scope string, now time.Time) (*RunView, error) {
	_ = now
	rp, err := cache.get(id)
	if err != nil {
		return nil, err
	}
	store := cache.store
	v := &RunView{ID: id, Replay: rp, Live: store.RunLive(id), RunDir: store.RunDir(id), Scope: scope}
	if scope == ScopeThisRun {
		v.Stats = runStats(rp)
	} else {
		v.Stats = h[rp.Header.Workflow]
	}
	targets, cerr := irs.get(rp.Header.ConfigPath)
	if cerr != nil {
		v.Err = cerr
	} else {
		v.IR = targets[rp.Header.Workflow]
	}
	report, err := (pipeline.RunReporter{Targets: targets}).Report(rp, v.IR, filepath.Join(v.RunDir, "journal.jsonl"))
	if err != nil {
		return nil, err
	}
	v.Report = report
	v.Running = runningOf(rp)
	v.Bounds = config.LoopBounds(v.IR)
	for id := range rp.LastByStep {
		config.NoteLoopBounds(v.Bounds, id)
	}
	for _, s := range rp.Starts {
		config.NoteLoopBounds(v.Bounds, s.StepID)
	}
	return v, nil
}

// previewWorkflow builds a RunView over the IR alone — an empty replay
// reported through the same reporter — so the Run screen's tree and flow
// show a workflow before any run exists. Every line is
// "absent", which the preview renders as structure rather than state.
func previewWorkflow(p *Project, name string) (*RunView, error) {
	if p == nil || p.cfg == nil {
		return nil, fmt.Errorf("preview %s: the config does not validate", name)
	}
	ir := p.irs[name]
	if ir == nil {
		return nil, fmt.Errorf("preview %s: workflow not desugared", name)
	}
	rp := &failure.Replay{Header: failure.Header{Workflow: name}, Results: map[failure.Key]failure.ResultRecord{}, LastByStep: map[string]failure.ResultRecord{}}
	report, err := (pipeline.RunReporter{Targets: p.irs}).Report(rp, ir, "")
	if err != nil {
		return nil, err
	}
	return &RunView{ID: name, Replay: rp, Report: report, IR: ir, Bounds: config.LoopBounds(ir), Stats: p.stats[name], Scope: ScopeAllRuns}, nil
}

// outputTail reads the last n lines of an attempt's output.log.
func outputTail(runDir, nodeID string, attempt, n int) []string {
	if n <= 0 {
		return nil
	}
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
	id = strings.TrimSuffix(id, "/") // an iteration prefix names the iteration, not a child
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
