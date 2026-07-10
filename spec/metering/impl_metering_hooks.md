# Implementation: Metering hook design

Covers MeterInterface and EndpointTiers.

## Core types (internal/metering/meter.go)

```go
type Unit string // opaque: "tokens", "usd-cents", "gpu-seconds" — user vocabulary

type Cost struct {
    Unit   Unit  `json:"unit"`
    Amount int64 `json:"amount"` // atomic granularity of the unit; no floats
}

func (c Cost) Add(o Cost) (Cost, error) // error on unit mismatch, never coerce

type Step struct {          // the estimate view: resolved, launch-ready
    NodeID   string
    Template string
    Endpoint string            // endpoint class from the metering config
    Inputs   map[string]any    // resolved input values
    Prompt   PromptSource      // skill + context-bundle reader for tokenizers
}

type ResultView struct {    // the actual view: result record + usage sidecar
    NodeID  string
    Status  string            // ok | failed
    Usage   map[string]int64  // decoded usage sidecar; nil if none deposited
    Elapsed time.Duration
}

type Estimate struct {
    Costs      []Cost     // unit-tagged upper bounds; zero amounts participate
    DeferUntil *time.Time // saturation hint (probe tier); nil otherwise
}

type Meter interface {
    Estimate(ctx context.Context, s Step) (Estimate, error)
    Actual(ctx context.Context, r ResultView) ([]Cost, error)
}
```

The default meter set is empty — `Admitter` with no meters and no budgets is
the no-op path and allocates nothing per step.

## Admitter (internal/metering/admit.go)

```go
type Decision struct {
    Kind   Kind      // Admit | Defer | Reject
    Until  time.Time // Defer only; zero => re-check on next settlement
    Budget Unit      // Reject only: the exhausted unit
    Detail string
}

type Admitter struct {
    mu       sync.Mutex
    meters   map[string][]Meter // endpoint class -> meters
    limit    map[Unit]int64     // from --budget unit=n
    spent    map[Unit]int64     // settled actuals
    reserved map[Unit]int64     // in-flight estimates
    held     map[string][]Cost  // node id -> reservation to release
}

func (a *Admitter) Admit(ctx context.Context, s Step) (Decision, error)
func (a *Admitter) Settle(ctx context.Context, r ResultView) ([]Cost, error)
```

`Admit` and `Settle` serialize on the mutex — admission reads a consistent
ledger, and two concurrent ready steps cannot both reserve the last headroom.
`Admit`: gather estimates from `meters[s.Endpoint]`; latest `DeferUntil` wins;
then per declared budget unit, with `est` summed over costs tagged that unit
from meters that claimed it: `spent+est > limit` ⇒ Reject; `spent+reserved+est
> limit` ⇒ zero-`Until` Defer; else hold `held[s.NodeID]` and Admit. At
construction, any budget unit no meter reports logs the never-enforced
warning. `Settle`: release the reservation, call each meter's `Actual`, fold
into `spent`, return the costs for the step's journal record (the aggregation
seam's raw material). An `Estimate` error from a budget-participating meter is
returned to the scheduler as a structured admission failure — a hard bound
that cannot be computed must not silently admit.

## Tier implementations (internal/metering/tiers.go)

All subprocess work goes through one typed interface (fake-able in tests),
matching the engine-wide os/exec discipline:

```go
type ProbeRunner interface { // structured I/O only
    Run(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error)
}
```

- **exactMeter**: `Estimate` streams the assembled prompt to the configured
  tokenizer command, decodes `{"tokens": n}`, returns `n + maxOutput` in the
  tier's unit. Decode or nonzero-exit error ⇒ error (admission failure).
  `Actual` prefers `Usage`, falls back to the bound.
- **reportedMeter**: `Estimate` returns `[]Cost{{unit, 0}}`. `Actual` maps
  configured sidecar fields to costs (`fields: {input_tokens: tokens, ...}`);
  a missing or malformed sidecar is a logged warning and zero cost —
  accounting is best-effort, never a step failure.
- **probeMeter**: `Estimate` runs the probe command, decodes
  `{"utilization": f, "reset": epoch?}`; `f >= threshold` ⇒ `DeferUntil`
  (reset, or now+default when absent). Probe error ⇒ warn and admit
  (best-effort by contract). `Actual` returns nil.

## Metering config file (internal/metering/config.go)

Supplied at run entry (a CLI flag naming the file); absent ⇒ empty meter set.
Decoded with yaml.v3 into per-class entries:

```yaml
endpoints:
  agent-api:
    tier: reported            # exact | reported | probe
    unit: tokens
    fields: {input_tokens: tokens, output_tokens: tokens}
    templates: [implement, review, fix]   # default: all
  local-llm:
    tier: exact
    unit: tokens
    tokenizer: {command: ./hooks/tokenize, max_output: 8192}
```

Validation at load: known tier names, tier-required keys present, template
names resolve against the config, one class per template. Probe commands are
opaque user paths — faber validates presence, never content.
