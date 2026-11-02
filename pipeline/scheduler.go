package pipeline

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/failure"
	"github.com/dmitriyb/faber/metering"
)

// idHeap is the deterministic ready queue: a min-heap of node ids, so
// dispatch order among simultaneously ready nodes is stable name order.
type idHeap []string

func (h idHeap) Len() int           { return len(h) }
func (h idHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h idHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *idHeap) Push(x any)        { *h = append(*h, x.(string)) }
func (h *idHeap) Pop() any          { old := *h; n := len(old); v := old[n-1]; *h = old[:n-1]; return v }
func (h *idHeap) push(id string)    { heap.Push(h, id) }
func (h *idHeap) pop() string       { return heap.Pop(h).(string) }

// event is a worker-to-loop message. The loop goroutine owns every piece of
// graph state; workers communicate only through these.
type event interface{ isEvent() }

type evSettled struct {
	id       string
	res      failure.Result
	costs    []metering.Cost
	executed bool // a box ran (cost record due); false for cheap settlements
	slot     bool // release a box slot
}

type evDeferred struct {
	id        string
	until     time.Time // zero => re-check on the next settlement
	detail    string
	history   []failure.AttemptInfo // real failed attempts consumed before the defer
	usedDelta int
	costs     []metering.Cost // settled costs of the attempts behind the defer
	slot      bool
}

type evExpanded struct {
	id    string
	insts []instance
	err   error
}

type evWake struct{ id string }

// evExpected is the result of a loop's expect command, run for the node
// whose dispatch entered a new iteration.
type evExpected struct {
	id    string // the node whose dispatch triggered the check
	loop  string
	iter  int
	count int
	err   error
}

func (evSettled) isEvent()  {}
func (evDeferred) isEvent() {}
func (evExpanded) isEvent() {}
func (evWake) isEvent()     {}
func (evExpected) isEvent() {}

// admissionMeter is the slice of the metering admitter the scheduler calls.
// *metering.Admitter satisfies it; tests fake it to force adverse
// admission/settlement event orderings.
type admissionMeter interface {
	Admit(ctx context.Context, s metering.Step) (metering.Decision, error)
	Settle(ctx context.Context, r metering.ResultView) ([]metering.Cost, error)
}

var _ admissionMeter = (*metering.Admitter)(nil)

// scheduler drives every node of the flattened graph to a terminal state:
// Kahn-style readiness with deterministic (node-id) tie-breaks, per-node
// gates in fixed order (condition, journal, admission, box), defer(until)
// re-queueing, failure propagation, and generate splices. All state mutation
// happens on the Run goroutine; box and expansion work runs in workers.
type scheduler struct {
	g      *graph
	ce     *condEval
	scopes map[string]*scopeEnv // sub entry id -> its inner scope

	ready idHeap // cheap-gate dispatch queue
	slotQ idHeap // box-needing nodes waiting for a slot

	maxPar    int // <= 0 means unlimited
	slotsUsed int
	// genPools bounds concurrently running instances per generate node
	// (GenSpec.MaxParallel); lanes bounds boxes per declared lane
	// (IR.Lanes, taken by a template's Lane). Both are granted in drain,
	// in the same id order as the global slot, and released with it.
	genPools map[string]*pool
	lanes    map[string]*pool
	// loops holds every loop expectation by prefix id; checked records the
	// highest iteration each loop's command ran for; expected the last
	// count it reported. pauseDetail is the cap stop's message when the
	// scheduler, not the operator's marker, paused the run.
	loops       map[string]config.LoopSpec
	checked     map[string]int
	expected    map[string]int
	pauseDetail string

	events      chan event
	done        chan struct{}
	outstanding int
	waitZero    []string // zero-until deferred nodes, re-checked on settlement

	seed    *failure.RunSeed
	journal *failure.Journal
	policy  *failure.Policy
	admit   admissionMeter
	mcfg    *metering.Config
	rld     *metering.RateLimitDefer
	boxes   BoxRunner
	images  ImageTagger
	sink    EventSink // progress events; nil = none
	expand  *expander
	clock   Clock
	log     *slog.Logger

	runID  string
	runDir string
	// sessions is the per-run transcript-capture toggle threaded into every
	// box attempt (--sessions ORed with the journal header's flag on resume).
	sessions bool

	// pause probes the durable pause marker (failure.PauseRequested over the
	// run dir); nil never pauses (unit wiring). paused latches the first true
	// probe: a seen request stays honored for the rest of the execution even
	// if the marker file vanishes, so the drain decision cannot flap mid-run.
	// pausePoll is the idle re-probe interval (0 ⇒ pausePollInterval): with
	// nothing outstanding the loop blocks on events, and only this tick lets
	// a pause request end the run instead of waiting out a defer window.
	pause     func() bool
	paused    bool
	pausePoll time.Duration

	// failed counts nodes settled failed by THIS execution — the exit-code
	// source of truth, owned by the loop goroutine rather than re-derived by
	// re-reading the journal at report time (a report-load failure must not
	// turn a failed run into exit 0).
	failed int
	// halts records nodes settled halted by this execution, in settle order —
	// the executor's exit-code and run-end input, owned like failed.
	halts []HaltedStep

	fatal error
}

// HaltedStep is one halted settlement: the step and the halter's reason, as
// the executor's typed halt error surfaces them.
type HaltedStep struct {
	Step   string
	Reason string
}

// run executes the graph to quiescence. It returns non-nil only for
// process-level aborts (context cancellation, a journal that refuses
// appends); per-node failures are records, not errors — the executor reads
// them from the report.
func (s *scheduler) run(ctx context.Context) error {
	defer close(s.done)
	for _, id := range s.g.sortedIDs() {
		if s.g.indeg[id] == 0 {
			s.push(id)
		}
	}
	s.drain(ctx)
	for s.g.done < len(s.g.nodes) {
		if s.fatal != nil {
			if s.outstanding == 0 {
				break
			}
			s.step(<-s.events)
			continue
		}
		if s.paused && s.outstanding == 0 {
			// The pause drain ended the run: everything in flight settled and
			// journaled normally, nothing new dispatches, and the remaining
			// nodes stay unjournaled — exactly where resume re-enters.
			break
		}
		select {
		case ev := <-s.events:
			s.step(ev)
		case <-ctx.Done():
			s.fatal = fmt.Errorf("pipeline: run aborted: %w", context.Cause(ctx))
			continue
		case <-s.pauseProbe():
			// Idle tick, no event: the drain below re-probes the marker, so
			// a pause requested during a quiet wait (a timed defer window
			// with no worker outstanding) ends the run within one interval.
		}
		if s.fatal == nil {
			s.drain(ctx)
		}
	}
	return s.fatal
}

// send delivers a worker event unless the loop has already exited (a fatal
// abort with no outstanding workers can strand late timer wakes; they must
// not block forever).
func (s *scheduler) send(ev event) {
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

// push queues a node for dispatch.
func (s *scheduler) push(id string) {
	n := s.g.nodes[id]
	if n == nil || (n.life != nsPending && n.life != nsDeferred) {
		return
	}
	n.life = nsQueued
	s.ready.push(id)
}

// pool is one bounded resource: a generate's instance cap or a lane.
type pool struct {
	max, used int
}

func (p *pool) free() bool { return p == nil || p.max <= 0 || p.used < p.max }

// slotFree reports whether a box slot is available.
func (s *scheduler) slotFree() bool { return s.maxPar <= 0 || s.slotsUsed < s.maxPar }

// genPool returns the node's generate pool, nil when unbounded.
func (s *scheduler) genPool(n *execNode) *pool {
	if n.gen == "" {
		return nil
	}
	return s.genPools[n.gen]
}

// lanePool returns the node's lane pool, nil when its template names none.
func (s *scheduler) lanePool(n *execNode) *pool {
	if n.n.Template == nil || n.n.Template.Lane == "" {
		return nil
	}
	return s.lanes[n.n.Template.Lane]
}

// holdReason names what keeps a slot-waiting node from launching, or ""
// when nothing does. Checked in the grant order (global, generate, lane).
func (s *scheduler) holdReason(n *execNode) string {
	if !s.slotFree() {
		return fmt.Sprintf("slot %d of %d", s.slotsUsed, s.maxPar)
	}
	if p := s.genPool(n); !p.free() {
		return fmt.Sprintf("generate %s %d of %d", n.gen, p.used, p.max)
	}
	if p := s.lanePool(n); !p.free() {
		return fmt.Sprintf("lane %s %d of %d", n.n.Template.Lane, p.used, p.max)
	}
	return ""
}

// takePools and releasePools bracket a launch: the global slot plus the
// node's generate pool and lane, always together.
func (s *scheduler) takePools(n *execNode) {
	s.takeSlot()
	if p := s.genPool(n); p != nil {
		p.used++
	}
	if p := s.lanePool(n); p != nil {
		p.used++
	}
}

func (s *scheduler) releasePools(n *execNode) {
	s.releaseSlot()
	if n == nil {
		return
	}
	if p := s.genPool(n); p != nil && p.used > 0 {
		p.used--
	}
	if p := s.lanePool(n); p != nil && p.used > 0 {
		p.used--
	}
}

func (s *scheduler) takeSlot() {
	if s.maxPar > 0 {
		s.slotsUsed++
	}
}

func (s *scheduler) releaseSlot() {
	if s.maxPar > 0 && s.slotsUsed > 0 {
		s.slotsUsed--
	}
}

// pausePollInterval is the default idle re-probe cadence for the pause
// marker — coarse on purpose: one stat per interval, and a pause is honored
// within roughly one interval even when the run is sitting out a rate-limit
// defer window with nothing in flight.
const pausePollInterval = time.Second

// pauseProbe returns the idle re-probe wake channel, or nil (never fires)
// when no probe is wired or the request is already latched — the select
// then waits on events alone.
func (s *scheduler) pauseProbe() <-chan time.Time {
	if s.pause == nil || s.paused {
		return nil
	}
	d := s.pausePoll
	if d == 0 {
		d = pausePollInterval
	}
	return time.After(d)
}

// pauseGate re-derives the durable pause request from the store, once per
// drain pass, and latches it. Checking here — the single admission point for
// slots and dispatch — is what makes pause cooperative: outstanding workers
// keep settling through the ordinary event path while nothing new starts.
func (s *scheduler) pauseGate() bool {
	if s.paused {
		return true
	}
	if s.pause != nil && s.pause() {
		s.paused = true
		s.log.Debug("pause requested; letting in-flight steps settle, dispatching nothing new")
		s.emit(Event{Kind: EventPauseRequested})
	}
	return s.paused
}

// drain runs the loop-side work to a fixed point: grant slots to waiting box
// nodes in id order, then run the cheap gates for every ready node in id
// order. The slot grant order — not goroutine wakeup — decides which box
// launches next, keeping dispatch deterministic under a full semaphore.
// A pause request stops the pass before any grant or dispatch.
func (s *scheduler) drain(ctx context.Context) {
	for s.fatal == nil && !s.pauseGate() {
		progressed := false
		// One pass over the waiting nodes in id order: a node held by its
		// generate pool or lane steps aside without blocking the nodes
		// behind it; held nodes re-queue after the pass. The first time a
		// node is held it says why on the stream.
		var held []string
		for len(s.slotQ) > 0 && s.slotFree() && s.fatal == nil {
			id := s.slotQ.pop()
			n := s.g.nodes[id]
			if n == nil || n.life != nsSlotWait {
				continue
			}
			if why := s.holdReason(n); why != "" {
				s.announceHold(n, why)
				held = append(held, id)
				continue
			}
			n.waiting = ""
			s.takePools(n)
			s.launch(ctx, n)
			progressed = true
		}
		if !s.slotFree() {
			// Everything still queued waits on the global slot; say so once
			// for the head of the queue, which is the next to launch.
			if len(s.slotQ) > 0 {
				if n := s.g.nodes[s.slotQ[0]]; n != nil && n.life == nsSlotWait {
					s.announceHold(n, s.holdReason(n))
				}
			}
		}
		for _, id := range held {
			s.slotQ.push(id)
		}
		for len(s.ready) > 0 && s.fatal == nil {
			id := s.ready.pop()
			n := s.g.nodes[id]
			if n == nil || n.life != nsQueued {
				continue
			}
			s.dispatch(ctx, n)
			progressed = true
		}
		if !progressed {
			return
		}
	}
}

// dispatch runs one ready node's cheap gates in the fixed order: condition,
// then per-kind handling (journal lookup and admission happen on the agent
// path). Everything here is host-side and instantaneous; no slot is held.
func (s *scheduler) dispatch(ctx context.Context, n *execNode) {
	id := n.n.ID
	s.log.Debug("step ready", "step", id, "kind", n.n.Kind)
	if n.n.When != nil {
		ok, err := s.ce.eval(n.whenProg, n.n.When, s.depLookup, n.env.params)
		if err != nil {
			s.settleFailed(n, reasonCondition, err.Error())
			return
		}
		if !ok {
			s.settleSkip(n, StateSkippedCondition, "")
			return
		}
	}
	// The expect gate sits after the condition: an iteration the loop no
	// longer needs is skipped without running the command, so a converged
	// loop costs no extra calls.
	if loop, iter, ok := s.unexpectedIteration(id); ok {
		s.startExpect(ctx, n, loop, iter)
		return
	}
	switch n.n.Kind {
	case config.KindSelector:
		s.resolveSelector(n)
	case config.KindSubWorkflow:
		s.settleSubEntry(n)
	case config.KindGenerate:
		s.startExpansion(ctx, n)
	case config.KindAgent:
		s.dispatchAgent(n)
	default:
		s.settleFailed(n, reasonExpansion, fmt.Sprintf("unsupported node kind %q", n.n.Kind))
	}
}

func (s *scheduler) depLookup(id string) *execNode { return s.g.nodes[id] }

// dispatchAgent resolves inputs, keys the journal, and either adopts a prior
// ok record (resume semantics) or queues the node for a box slot.
func (s *scheduler) dispatchAgent(n *execNode) {
	if n.n.Template == nil {
		s.settleFailed(n, failure.ReasonLaunch, "agent node carries no resolved template")
		return
	}
	inputs, skippedSrc, err := resolveInputs(s.g, n)
	if err != nil {
		s.settleFailed(n, failure.ReasonLaunch, err.Error())
		return
	}
	if skippedSrc != "" {
		s.settleSkip(n, StateSkippedDependency, skippedSrc)
		return
	}
	n.inputs = inputs
	if s.images != nil {
		tag, err := s.images.Tag(n.n.Template)
		if err != nil {
			s.settleFailed(n, failure.ReasonLaunch, fmt.Sprintf("resolve image tag: %v", err))
			return
		}
		n.image = tag
	}
	hash, err := failure.InputHash(inputs, n.n.Template.Name, n.image)
	if err != nil {
		s.settleFailed(n, failure.ReasonLaunch, err.Error())
		return
	}
	n.hash = hash
	if s.seed != nil {
		prior, hit, err := s.seed.Lookup(n.n.ID, inputs, n.n.Template.Name, n.image)
		if err != nil {
			s.settleFailed(n, failure.ReasonLaunch, err.Error())
			return
		}
		if hit {
			// Adopt the journaled result verbatim: dependents thread the prior
			// payload, no box runs, no cost record. The fresh record carries a
			// cached annotation so the report (journal-derived) can mark it.
			n.cached = true
			now := s.clock.Now()
			prior.Attempts = append(prior.Attempts, failure.AttemptInfo{
				Timing: failure.Timing{Started: now, Finished: now},
				Error:  &failure.ErrorRecord{Reason: reasonCached, Detail: "journal hit; result adopted without execution"},
			})
			s.settleResult(n, prior, false, nil)
			return
		}
	}
	if s.boxes == nil {
		s.settleFailed(n, failure.ReasonLaunch, "no box runner is wired into this executor")
		return
	}
	n.life = nsSlotWait
	s.slotQ.push(n.n.ID)
}

// settleSubEntry binds an inlined sub-workflow's parameter scope from the
// entry node's bindings and inbound data edges, then settles the entry ok —
// pure wiring, no container. The entry's ordering edges release the inner
// sources only after the scope params exist.
func (s *scheduler) settleSubEntry(n *execNode) {
	env := s.scopes[n.n.ID]
	if env == nil {
		s.settleFailed(n, reasonExpansion, "sub-workflow scope was never registered")
		return
	}
	for _, name := range sortedKeys(n.n.Bindings) {
		v, ok, err := resolveBinding(n.n.Bindings[name], n.env)
		if err != nil {
			s.settleFailed(n, reasonExpansion, fmt.Sprintf("param %q: %v", name, err))
			return
		}
		if ok {
			env.params[name] = v
		}
	}
	for _, e := range s.g.dataIn[n.n.ID] {
		src := s.g.nodes[e.From]
		if src == nil || !src.terminal() {
			s.settleFailed(n, reasonExpansion, fmt.Sprintf("param %q: source %s has not settled", e.ToPort, e.From))
			return
		}
		switch src.status {
		case StateOK:
			v, ok := src.payload[e.FromPort]
			if !ok {
				s.settleFailed(n, reasonExpansion, fmt.Sprintf("param %q: source %s settled without field %q", e.ToPort, e.From, e.FromPort))
				return
			}
			env.params[e.ToPort] = v
		default:
			s.settleSkip(n, StateSkippedDependency, e.From)
			return
		}
	}
	now := s.clock.Now()
	s.settleResult(n, failure.Result{
		Status:  failure.StatusOK,
		Payload: json.RawMessage(`{}`),
		Timing:  failure.Timing{Started: now, Finished: now},
		Attempt: 1,
	}, false, nil)
}

// resolveSelector settles a post-loop selector as pure wiring: adopt the
// newest executed candidate's payload; if the final iteration executed and
// the loop's exhaustion condition holds, the loop settled without its until
// predicate — the bounded-loop failure. No container, no meter, no slot.
func (s *scheduler) resolveSelector(n *execNode) {
	sel := n.n.Sel
	if sel == nil || len(sel.Candidates) == 0 {
		s.settleFailed(n, reasonExpansion, "selector node carries no candidates")
		return
	}
	final := s.g.nodes[sel.Candidates[0]]
	if final != nil && final.status == StateOK && n.exhProg != nil {
		exhausted, err := s.ce.eval(n.exhProg, sel.Exhausted, s.depLookup, n.env.params)
		if err != nil {
			s.settleFailed(n, reasonCondition, err.Error())
			return
		}
		if exhausted {
			s.settleFailed(n, failure.ReasonLoopExhausted, fmt.Sprintf(
				"all %d iterations of step %q executed without the loop's until condition settling",
				len(sel.Candidates), sel.Step))
			return
		}
	}
	for _, c := range sel.Candidates {
		cn := s.g.nodes[c]
		if cn == nil || cn.status != StateOK {
			continue
		}
		now := s.clock.Now()
		s.settleResult(n, failure.Result{
			Status:  failure.StatusOK,
			Payload: cn.raw,
			Timing:  failure.Timing{Started: now, Finished: now},
			Attempt: 1,
		}, false, nil)
		return
	}
	// Every candidate was skipped: the loop body never executed, so the
	// selector has no value and skip propagates to whatever reads it.
	s.settleSkip(n, StateSkippedCondition, "")
}

// unexpectedIteration finds the outermost loop with an expect command whose
// iteration this node enters for the first time — the boundary the command
// must run at before the node dispatches.
func (s *scheduler) unexpectedIteration(id string) (loop string, iter int, ok bool) {
	if len(s.loops) == 0 {
		return "", 0, false
	}
	segs := strings.Split(id, "/")
	for k, seg := range segs {
		name, i, isIter := config.IterationOf(seg)
		if !isIter {
			continue
		}
		loop = strings.Join(append(append([]string{}, segs[:k]...), name), "/")
		if _, declared := s.loops[loop]; declared && i > s.checked[loop] {
			return loop, i, true
		}
	}
	return "", 0, false
}

// startExpect runs the loop's expect command in a worker (host-side, no
// slot) and folds the count on the loop goroutine; the node stays queued
// (nsRunning here only means "a worker is out for it") until the verdict.
func (s *scheduler) startExpect(ctx context.Context, n *execNode, loop string, iter int) {
	n.life = nsRunning
	s.outstanding++
	spec := s.loops[loop]
	env := n.env
	go func() {
		count, err := s.expand.expectCount(ctx, spec, env)
		s.send(evExpected{id: n.n.ID, loop: loop, iter: iter, count: count, err: err})
	}()
}

// startExpansion runs the generate node's data source in a worker (host-side
// work, no slot) and applies the splice on the loop goroutine.
func (s *scheduler) startExpansion(ctx context.Context, n *execNode) {
	n.life = nsRunning
	s.outstanding++
	node, env := n.n, n.env
	go func() {
		insts, err := s.expand.expand(ctx, node, env)
		s.send(evExpanded{id: node.ID, insts: insts, err: err})
	}()
}

// boxJob is the immutable snapshot a box worker runs from.
type boxJob struct {
	id        string
	inputs    map[string]any
	hash      string
	image     string
	tpl       *config.ResolvedTemplate
	retry     int
	onFailure string
	endpoint  string
	base      int // real attempts consumed by earlier (deferred) dispatches
}

// launch starts the box worker for a slot-granted agent node.
func (s *scheduler) launch(ctx context.Context, n *execNode) {
	n.life = nsRunning
	s.log.Debug("step dispatched", "step", n.n.ID, "template", templateName(n.n.Template), "attempts_used", n.used)
	retry := n.n.Retry - n.used
	if retry < 0 {
		retry = 0
	}
	endpoint := ""
	if s.mcfg != nil && n.n.Template != nil {
		endpoint = s.mcfg.ClassForTemplate(n.n.Template.Name)
	}
	job := boxJob{
		id:        n.n.ID,
		inputs:    n.inputs,
		hash:      n.hash,
		image:     n.image,
		tpl:       n.n.Template,
		retry:     retry,
		onFailure: n.n.OnFailure,
		endpoint:  endpoint,
		base:      n.used,
	}
	s.outstanding++
	go s.boxWorker(ctx, job)
}

// deferEscape is the synthetic record a converted rate-limit defer returns to
// break out of the failure policy's retry loop without consuming an attempt.
// It is never journaled or threaded; the worker discards it.
func deferEscape() failure.Result {
	return failure.Result{Status: failure.StatusOK, Payload: json.RawMessage(`{"deferred":true}`)}
}

// boxWorker drives one admitted dispatch of an agent node: meter admission,
// then the failure policy's attempt loop over the box-run seam, with the
// metering settle hook at every attempt completion and the rate-limit defer
// floor converting 429-class failures into re-queues that consume no retry.
func (s *scheduler) boxWorker(ctx context.Context, job boxJob) {
	step := metering.Step{
		NodeID:   job.id,
		Template: templateName(job.tpl),
		Endpoint: job.endpoint,
		Inputs:   job.inputs,
	}
	dec, err := s.admit.Admit(ctx, step)
	if err != nil {
		s.send(evSettled{id: job.id, slot: true, res: failure.Result{
			Status:  failure.StatusFailed,
			Error:   &failure.ErrorRecord{Reason: reasonAdmission, Detail: err.Error()},
			Attempt: 1,
		}})
		return
	}
	switch dec.Kind {
	case metering.Defer:
		s.send(evDeferred{id: job.id, until: dec.Until, detail: dec.Detail, slot: true})
		return
	case metering.Reject:
		s.send(evSettled{id: job.id, slot: true, res: failure.Result{
			Status:  failure.StatusFailed,
			Error:   &failure.ErrorRecord{Reason: reasonBudget, Detail: dec.Detail},
			Attempt: 1,
		}})
		return
	}

	var costs []metering.Cost
	var deferDec *metering.Decision
	var deferErr *failure.ErrorRecord
	var budgetStop *failure.ErrorRecord
	deferAttempt := 0
	runner := func(ctx context.Context, attempt int) (failure.Result, error) {
		if attempt > 1 {
			// Every attempt is admitted, not just the dispatch: the first
			// attempt's reservation settled with its actuals, so a retry
			// without re-admission would run outside the budget ledger and
			// its Settle would record nothing (no reservation to key off).
			dec, aerr := s.admit.Admit(ctx, step)
			if aerr != nil {
				budgetStop = &failure.ErrorRecord{Reason: reasonAdmission, Detail: aerr.Error()}
				return deferEscape(), nil
			}
			switch dec.Kind {
			case metering.Defer:
				// Re-queue without consuming this retry, exactly like a
				// rate-limit defer; the earlier attempts' history rides along.
				deferDec, deferAttempt = &dec, job.base+attempt
				return deferEscape(), nil
			case metering.Reject:
				// A budget rejection is terminal — spent only grows, so
				// retrying the admission would burn the remaining attempts
				// on no-op refusals.
				budgetStop = &failure.ErrorRecord{Reason: reasonBudget, Detail: dec.Detail}
				return deferEscape(), nil
			}
		}
		// The attempt's start is durable state like a defer is: journaled the
		// moment it launches, so a reader sees "running since" rather than
		// nothing until settlement. A failed append is not fatal here — the
		// settlement's own append surfaces an unappendable journal.
		started := s.clock.Now()
		if err := s.journal.AppendStart(failure.StartRecord{StepID: job.id, Attempt: job.base + attempt, Started: started}); err != nil {
			s.log.Warn("journal start record", "step", job.id, "err", err)
		}
		s.emit(Event{Kind: EventStepStarted, Time: started, Step: job.id, Template: templateName(job.tpl), Attempt: job.base + attempt})
		br, runErr := s.boxes.RunAttempt(ctx, BoxAttempt{
			RunID:    s.runID,
			RunDir:   s.runDir,
			NodeID:   job.id,
			Attempt:  job.base + attempt,
			Template: job.tpl,
			Image:    job.image,
			Inputs:   job.inputs,
			Sessions: s.sessions,
		})
		res := br.Result
		view := metering.ResultView{NodeID: job.id, Status: metering.StatusFailed, Usage: br.Usage}
		if runErr == nil {
			view.Status = string(res.Status)
			view.Elapsed = res.Timing.Duration()
		}
		// Settle runs for every attempt, any status, before rate-limit
		// classification, so a waiting step never holds budget headroom.
		cc, serr := s.admit.Settle(ctx, view)
		if serr != nil {
			s.log.Warn("metering settle failed", "step", job.id, "err", serr)
		}
		costs = append(costs, cc...)
		if runErr == nil && res.Status == failure.StatusFailed && res.Error != nil && res.Error.Reason == metering.ReasonRateLimit {
			rec := metering.FailureRecord{
				NodeID: job.id,
				Status: metering.StatusFailed,
				Reason: res.Error.Reason,
				Detail: json.RawMessage(res.Error.Detail),
			}
			if d, ok := s.rld.Classify(rec, s.clock.Now()); ok {
				deferDec, deferErr, deferAttempt = &d, res.Error, job.base+attempt
				return deferEscape(), nil
			}
		}
		return res, runErr
	}
	res := s.policy.RunStep(ctx, failure.StepSpec{
		ID:        job.id,
		InputHash: job.hash,
		Inputs:    job.inputs,
		Retry:     job.retry,
		OnFailure: job.onFailure,
	}, runner)
	if res.Error != nil && res.Error.Reason == failure.ReasonCanceled {
		// The loop-head cancel returned before the first attempt's runner ran,
		// so the pre-loop reservation was never settled. Release it (Settle is
		// idempotent — a no-op if an attempt already settled) so the
		// per-execution ledger's "every reservation settles" invariant holds
		// even on abort. Its cost return is discarded: no attempt executed.
		_, _ = s.admit.Settle(ctx, metering.ResultView{NodeID: job.id, Status: metering.StatusFailed})
	}
	if deferDec != nil {
		// The converted attempt consumed no retry; the policy saw a synthetic
		// success, so its between-attempt cleanup did not run — run it here
		// before the node re-queues (clean slate for the re-admitted attempt).
		s.cleanupAfterDefer(ctx, job, deferErr, deferAttempt)
		s.send(evDeferred{
			id:        job.id,
			until:     deferDec.Until,
			detail:    deferDec.Detail,
			history:   res.Attempts,
			usedDelta: len(res.Attempts),
			costs:     costs, // settled attempt costs survive the re-queue
			slot:      true,
		})
		return
	}
	if budgetStop != nil {
		// A retry's re-admission refused terminally: the escape rode a
		// synthetic success through the policy, so re-shape the settlement as
		// the budget failure carrying the real attempts' history and costs.
		now := s.clock.Now()
		res = failure.Result{
			Status:   failure.StatusFailed,
			Error:    budgetStop,
			Timing:   failure.Timing{Started: now, Finished: now},
			Attempt:  len(res.Attempts) + 1,
			Attempts: res.Attempts,
		}
		s.send(evSettled{id: job.id, res: res, costs: costs, executed: true, slot: true})
		return
	}
	if res.Status != failure.StatusFailed {
		// ok and halted both settle decisively: neither leaves a consecutive
		// rate-limit streak for the defer floor to keep counting.
		s.rld.Reset(job.id)
	}
	s.send(evSettled{id: job.id, res: res, costs: costs, executed: true, slot: true})
}

// cleanupAfterDefer runs the step's declared on_failure hook for a
// defer-converted attempt and journals the cleanup outcome, mirroring the
// failure policy's between-attempt cleanup.
func (s *scheduler) cleanupAfterDefer(ctx context.Context, job boxJob, errRec *failure.ErrorRecord, attempt int) {
	if job.onFailure == "" || errRec == nil {
		return
	}
	// Detached-and-bounded only when the run is already aborted, exactly like
	// the policy's own cleanup: a healthy run keeps the live context.
	hctx, cancel := failure.CleanupContext(ctx)
	defer cancel()
	var err error
	if s.policy.Hooks == nil {
		err = fmt.Errorf("step declares on_failure but no hook runner is wired")
	} else {
		err = s.policy.Hooks.RunOnFailure(hctx, failure.HookInvocation{
			Script:  job.onFailure,
			StepID:  job.id,
			Attempt: attempt,
			Inputs:  job.inputs,
			Error:   *errRec,
		})
	}
	rec := failure.CleanupRecord{StepID: job.id, InputHash: job.hash, Attempt: attempt, OK: err == nil}
	if err != nil {
		rec.Detail = err.Error()
		s.log.Warn("on_failure cleanup after defer failed", "step", job.id, "attempt", attempt)
	}
	if aerr := s.journal.AppendCleanup(rec); aerr != nil {
		s.log.Error("journal cleanup record", "step", job.id, "err", aerr)
	}
}

// step folds one worker event into the graph on the loop goroutine.
func (s *scheduler) step(ev event) {
	switch ev := ev.(type) {
	case evSettled:
		s.outstanding--
		n := s.g.nodes[ev.id]
		if ev.slot {
			s.releasePools(n)
		}
		if n == nil || n.terminal() {
			return
		}
		res := ev.res
		res.Attempt += n.used
		merged := make([]failure.AttemptInfo, 0, len(n.history)+len(res.Attempts)+len(n.annotations))
		merged = append(merged, n.history...)
		merged = append(merged, res.Attempts...)
		merged = append(merged, n.annotations...)
		if len(merged) > 0 {
			res.Attempts = merged
		}
		// Only this dispatch's costs are passed; the node's stashed costs
		// (from earlier defer-converted dispatches) are folded inside
		// settleResult, so they survive whichever settle path fires.
		s.settleResult(n, res, ev.executed, ev.costs)
	case evDeferred:
		s.outstanding--
		n := s.g.nodes[ev.id]
		if ev.slot {
			s.releasePools(n)
		}
		if n == nil || n.terminal() {
			return
		}
		n.used += ev.usedDelta
		n.history = append(n.history, ev.history...)
		n.costs = append(n.costs, ev.costs...)
		now := s.clock.Now()
		finish := ev.until
		if finish.IsZero() {
			finish = now
		}
		n.annotations = append(n.annotations, failure.AttemptInfo{
			Timing: failure.Timing{Started: now, Finished: finish},
			Error:  &failure.ErrorRecord{Reason: reasonDeferred, Detail: ev.detail},
		})
		n.life = nsDeferred
		// The waiting state is durable state: the defer is journaled when it
		// happens, so a crash mid-wait leaves the timeline on disk instead of
		// losing it with the never-settled node.
		if err := s.journal.AppendDefer(failure.DeferRecord{StepID: ev.id, Until: ev.until, Detail: ev.detail}); err != nil && s.fatal == nil {
			s.fatal = fmt.Errorf("pipeline: journal became unappendable; aborting the run: %w", err)
		}
		s.log.Debug("step deferred", "step", ev.id, "until", ev.until, "detail", ev.detail)
		s.emit(Event{Kind: EventStepDeferred, Time: now, Step: ev.id, Until: ev.until, Detail: ev.detail})
		if ev.until.IsZero() {
			s.waitZero = append(s.waitZero, ev.id)
			// The settlement this defer is waiting on may already have been
			// folded — its evSettled can race ahead of this evDeferred. With
			// nothing left in flight no future settlement will drain the
			// parking lot, so re-check immediately rather than hang.
			if s.outstanding == 0 {
				s.wakeParked()
			}
			return
		}
		id := ev.id
		s.clock.AfterFunc(ev.until.Sub(now), func() { s.send(evWake{id: id}) })
		// A timed defer released its slot and settled its attempts' costs —
		// exactly the state change a zero-until parked node waits to re-check.
		// Without this wake, parked admissible work would stall for the whole
		// rate-limit window (a timed defer is not a settlement, and nothing
		// else may be in flight). The just-deferred node itself is not in the
		// parking lot (it has a timer), so no self-wake loop.
		s.wakeParked()
	case evWake:
		n := s.g.nodes[ev.id]
		if n != nil && n.life == nsDeferred {
			s.push(ev.id)
		}
	case evExpanded:
		s.outstanding--
		s.applyExpansion(ev)
	case evExpected:
		s.outstanding--
		s.applyExpected(ev)
	}
}

// applyExpected folds a loop's expected count: a command failure fails the
// triggering node with the source-contract reason (dependents cascade like
// any failure); a count over the cap pauses the run at this boundary with
// the message resume needs; otherwise the node goes back to the ready queue
// and the boundary is recorded so the command does not run again for it.
func (s *scheduler) applyExpected(ev evExpected) {
	n := s.g.nodes[ev.id]
	if n == nil || n.terminal() {
		return
	}
	if ev.iter > s.checked[ev.loop] {
		s.checked[ev.loop] = ev.iter
	}
	if ev.err != nil {
		s.log.Warn("loop expect command failed", "loop", ev.loop, "err", ev.err)
		res := sourceContractResult(ev.err)
		now := s.clock.Now()
		res.Timing = failure.Timing{Started: now, Finished: now}
		s.settleResult(n, res, false, nil)
		return
	}
	spec := s.loops[ev.loop]
	s.expected[ev.loop] = ev.count
	s.emit(Event{Kind: EventLoopExpected, Step: ev.loop, Expected: ev.count, Cap: spec.Max})
	n.life = nsPending
	if ev.count > spec.Max {
		if !s.paused {
			s.paused = true
			s.pauseDetail = fmt.Sprintf("%s: expected %d, cap %d; raise the cap and resume", ev.loop, ev.count, spec.Max)
			s.log.Debug("loop expectation exceeds the cap; pausing", "loop", ev.loop, "expected", ev.count, "cap", spec.Max)
			s.emit(Event{Kind: EventPauseRequested, Detail: s.pauseDetail})
		}
		// The node stays undispatched: a resumed run re-enters exactly here.
		return
	}
	s.push(n.n.ID)
}

// applyExpansion splices the generate node's instances into the running DAG
// atomically: nodes and edges added, in-degrees seeded, inter-instance
// ordering derived from item deps, and the node's original dependents rewired
// to also wait on every instance's sinks — all before the generate node
// itself settles ok, so no dependent can slip between expansion and rewiring.
func (s *scheduler) applyExpansion(ev evExpanded) {
	n := s.g.nodes[ev.id]
	if n == nil || n.terminal() {
		return
	}
	if ev.err != nil {
		s.log.Warn("generate expansion failed", "step", ev.id, "err", ev.err)
		res := sourceContractResult(ev.err)
		now := s.clock.Now()
		res.Timing = failure.Timing{Started: now, Finished: now}
		s.settleResult(n, res, false, nil)
		return
	}

	var newIDs []string
	instByID := map[string]instance{}
	for _, inst := range ev.insts {
		ids, err := s.g.addLevel(inst.ir, inst.env, s.ce)
		newIDs = append(newIDs, ids...)
		if err != nil {
			s.g.remove(newIDs)
			s.settleFailed(n, reasonExpansion, err.Error())
			return
		}
		s.registerScopes(inst.ir)
		instByID[inst.itemID] = inst
	}
	// Every spliced node belongs to this generate, whose instance bound (if
	// any) is one pool shared by all of them.
	for _, id := range newIDs {
		if gn := s.g.nodes[id]; gn != nil {
			gn.gen = ev.id
		}
	}
	if n.n.Gen != nil && n.n.Gen.MaxParallel > 0 {
		s.genPools[ev.id] = &pool{max: n.n.Gen.MaxParallel}
	}
	// An instance's loops carry their expectations under the instance scope.
	for _, inst := range ev.insts {
		for id, l := range loopSpecs(inst.ir, ev.id+"["+inst.itemID+"]/") {
			s.loops[id] = l
		}
	}
	// Inter-instance ordering: every sink of the dep's instance orders every
	// source of this instance. Deps outside the set were dropped upstream.
	for _, inst := range ev.insts {
		for _, dep := range inst.deps {
			dj, ok := instByID[dep]
			if !ok {
				continue
			}
			for _, sink := range deepSinks(dj.ir) {
				for _, src := range levelSources(inst.ir) {
					s.g.edge(sink, src)
				}
			}
		}
	}
	// "After the fan-out" means after every instance settles: rewire the
	// generate node's dependents onto every instance's sinks.
	dependents := append([]string(nil), s.g.succ[ev.id]...)
	for _, inst := range ev.insts {
		for _, sink := range deepSinks(inst.ir) {
			for _, d := range dependents {
				s.g.edge(sink, d)
			}
		}
	}
	// Instance sources with no in-set deps are ready immediately, in node-id
	// order like everything else.
	sort.Strings(newIDs)
	for _, id := range newIDs {
		if s.g.indeg[id] == 0 {
			s.push(id)
		}
	}
	s.log.Debug("generate expanded", "step", ev.id, "items", len(ev.insts), "nodes", len(newIDs))
	caps := map[string]int{}
	for _, id := range newIDs {
		noteBounds(caps, id)
	}
	s.emit(Event{Kind: EventGenerateExpanded, Step: ev.id, Items: len(ev.insts), Nodes: len(newIDs), Loops: caps})
	now := s.clock.Now()
	s.settleResult(n, failure.Result{
		Status:  failure.StatusOK,
		Payload: generatePayload(ev.insts),
		Timing:  failure.Timing{Started: now, Finished: now},
		Attempt: 1,
	}, false, nil)
}

// registerScopes indexes the sub-workflow scopes addLevel created inside a
// spliced instance so their entries can bind params at settle time.
func (s *scheduler) registerScopes(ir *config.IR) {
	for i := range ir.Nodes {
		n := &ir.Nodes[i]
		if n.Kind == config.KindSubWorkflow && n.Sub != nil {
			if members := s.g.members[n.ID]; len(members) > 0 {
				s.scopes[n.ID] = s.g.nodes[members[0]].env
			} else {
				s.scopes[n.ID] = &scopeEnv{entry: n.ID, params: map[string]any{}}
			}
			s.registerScopes(n.Sub)
		}
	}
}

// settleFailed settles a node failed with a pipeline-produced reason.
func (s *scheduler) settleFailed(n *execNode, reason, detail string) {
	now := s.clock.Now()
	s.settleResult(n, failure.Result{
		Status:  failure.StatusFailed,
		Error:   &failure.ErrorRecord{Reason: reason, Detail: detail},
		Timing:  failure.Timing{Started: now, Finished: now},
		Attempt: 1,
	}, false, nil)
}

// settleSkip settles a node into one of the skip states. The journal
// encoding is a failed-status record carrying the skip reason (the failure
// module's record union has no third status) and a null input hash, so it is
// never a resume hit; the reporter decodes it back to the skip state. For
// dependency and halt skips the root ancestor's id travels in the detail.
func (s *scheduler) settleSkip(n *execNode, state, ancestor string) {
	now := s.clock.Now()
	res := failure.Result{
		Status:  failure.StatusFailed,
		Error:   &failure.ErrorRecord{Reason: state, Detail: ancestor},
		Timing:  failure.Timing{Started: now, Finished: now},
		Attempt: 1,
	}
	rec := failure.ResultRecord{StepID: n.n.ID, InputHash: "", Result: res}
	if err := s.journal.AppendResult(rec); err != nil && s.fatal == nil {
		s.fatal = fmt.Errorf("pipeline: journal became unappendable; aborting the run: %w", err)
	}
	s.emit(Event{Kind: EventStepSettled, Time: now, Step: n.n.ID, Status: state, Reason: ancestor})
	s.finish(n, state)
}

// settleResult journals one settled result and folds the terminal state into
// the graph. executed marks a real box execution, which additionally owes a
// cost record (cached adoptions and cheap settlements do not).
func (s *scheduler) settleResult(n *execNode, res failure.Result, executed bool, costs []metering.Cost) {
	status := StateOK
	switch res.Status {
	case failure.StatusFailed:
		status = StateFailed
	case failure.StatusHalted:
		status = StateHalted
	}
	if status == StateOK {
		payload, err := decodePayload(res.Payload)
		if err != nil {
			// Defensive: an undecodable ok payload cannot thread.
			res = failure.Result{
				Status:  failure.StatusFailed,
				Error:   &failure.ErrorRecord{Reason: failure.ReasonResultSchema, Detail: err.Error()},
				Timing:  res.Timing,
				Attempt: res.Attempt,
			}
			status = StateFailed
		} else {
			n.payload = payload
			n.raw = res.Payload
		}
	}
	hash := ""
	if executed || n.cached {
		hash = n.hash
	}
	rec := failure.ResultRecord{StepID: n.n.ID, InputHash: hash, Result: res}
	if err := s.journal.AppendResult(rec); err != nil && s.fatal == nil {
		s.fatal = fmt.Errorf("pipeline: journal became unappendable; aborting the run: %w", err)
	}
	// The node's stashed costs (settled by earlier defer-converted dispatches)
	// are folded here — the single settle point — so they survive whichever
	// path settles the node, not only evSettled. A cost record is owed
	// whenever a box executed on this dispatch or any prior dispatch settled
	// costs that would otherwise vanish (e.g. a defer followed by a reject or
	// a cheap-gate failure on re-dispatch).
	allCosts := append(append([]metering.Cost{}, n.costs...), costs...)
	if executed || len(allCosts) > 0 {
		raw, err := json.Marshal(allCosts)
		if err != nil || len(allCosts) == 0 {
			raw = []byte("[]")
		}
		if err := s.journal.AppendCost(failure.CostRecord{StepID: n.n.ID, InputHash: n.hash, Cost: raw}); err != nil && s.fatal == nil {
			s.fatal = fmt.Errorf("pipeline: journal became unappendable; aborting the run: %w", err)
		}
	}
	ev := Event{Kind: EventStepSettled, Step: n.n.ID, Status: status, Cached: n.cached}
	if s0, f0 := recordSpan(res); !s0.IsZero() && f0.After(s0) {
		ev.Duration = f0.Sub(s0)
	}
	if status == StateFailed && res.Error != nil {
		ev.Reason = res.Error.Reason
		s.log.Debug("step failed", "step", n.n.ID, "reason", res.Error.Reason, "attempts", res.Attempt)
	}
	if status == StateHalted && res.Halt != nil {
		// The halt tally is recorded at the single settle point so the
		// executor's exit decision and the run-end record cannot disagree
		// with what was journaled.
		s.halts = append(s.halts, HaltedStep{Step: n.n.ID, Reason: res.Halt.Reason})
		ev.Reason = res.Halt.Reason
		s.log.Debug("step halted", "step", n.n.ID, "reason", res.Halt.Reason)
	}
	s.emit(ev)
	s.finish(n, status)
}

// finish marks a node terminal, applies scope-skip for non-ok sub-workflow
// entries, propagates failure or halt to transitive dependents, decrements
// dependents (readiness), and wakes zero-until deferred nodes.
func (s *scheduler) finish(n *execNode, status string) {
	id := n.n.ID
	n.life = nsDone
	n.status = status
	s.g.done++
	if status == StateFailed {
		s.failed++
	}
	s.log.Debug("step settled", "step", id, "status", status, "cached", n.cached)

	if members := s.g.members[id]; len(members) > 0 && status != StateOK {
		// A sub-workflow entry that skipped or failed takes its whole inlined
		// graph with it: condition skips cascade as condition skips, halt
		// skips cascade as halt skips (a triage stop must not read as a
		// failure cascade inside the sub graph either), anything else as
		// dependency skips — the non-condition flavors naming the entry.
		memberState, ancestor := StateSkippedDependency, id
		switch status {
		case StateSkippedCondition:
			memberState, ancestor = StateSkippedCondition, ""
		case StateSkippedHalt:
			memberState = StateSkippedHalt
		}
		for _, m := range members {
			mn := s.g.nodes[m]
			if mn != nil && !mn.terminal() {
				s.settleSkip(mn, memberState, ancestor)
			}
		}
	}
	switch status {
	case StateFailed:
		s.propagate(id, StateSkippedDependency)
	case StateHalted:
		s.propagate(id, StateSkippedHalt)
	}
	for _, d := range s.g.succ[id] {
		s.g.indeg[d]--
		if dn := s.g.nodes[d]; dn != nil && s.g.indeg[d] == 0 && dn.life == nsPending {
			s.push(d)
		}
	}
	s.wakeParked()
}

// wakeParked re-queues every zero-until deferred node. Called on every
// settlement (a zero-until defer means "re-check on the next settlement") and
// when a zero-until defer arrives with nothing left in flight — the
// settlement it was waiting on raced ahead of it.
func (s *scheduler) wakeParked() {
	if len(s.waitZero) == 0 {
		return
	}
	waiting := s.waitZero
	s.waitZero = nil
	for _, w := range waiting {
		if wn := s.g.nodes[w]; wn != nil && wn.life == nsDeferred {
			s.push(w)
		}
	}
}

// propagate walks a failed or halted node's dependents breadth-first and
// settles every not-yet-settled transitive dependent into state (the skip
// flavor matching the root: skipped-dependency after a failure, skipped-halt
// after a halt) with the root ancestor recorded. Settled nodes stop the
// walk — their own settlement already handled their dependents — so
// independent branches and join points with healthy inputs are untouched.
func (s *scheduler) propagate(root, state string) {
	visited := map[string]bool{}
	queue := append([]string(nil), s.g.succ[root]...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		n := s.g.nodes[id]
		if n == nil || n.terminal() {
			continue
		}
		if n.life != nsPending && n.life != nsQueued {
			// In-flight or deferred nodes had every dependency satisfied
			// before launch; their own result settles them.
			continue
		}
		s.settleSkip(n, state, root)
		queue = append(queue, s.g.succ[id]...)
	}
}

// announceHold emits step-waiting once per distinct hold reason for a node
// — the renderer's "ready, waiting for lane landing" — and never repeats
// it on every drain pass.
func (s *scheduler) announceHold(n *execNode, why string) {
	if why == "" || n.waiting == why {
		return
	}
	n.waiting = why
	s.emit(Event{Kind: EventStepWaiting, Step: n.n.ID, Detail: why})
}

// emit delivers one progress event to the sink, if any. Safe from any
// goroutine: the sink contract requires concurrent safety.
func (s *scheduler) emit(ev Event) {
	if s.sink == nil {
		return
	}
	if ev.Time.IsZero() {
		ev.Time = s.clock.Now()
	}
	s.sink.Emit(ev)
}

func templateName(tpl *config.ResolvedTemplate) string {
	if tpl == nil {
		return ""
	}
	return tpl.Name
}
