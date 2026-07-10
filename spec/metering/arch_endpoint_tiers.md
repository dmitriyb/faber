# EndpointTiers — cost fidelity per endpoint class

## What it is

How well cost can be measured is a property of the endpoint a box's agent
talks to, not of faber: a local model server can be bounded exactly, a vendor
API reports usage after the fact, a subscription endpoint reports nothing at
all. Tiers make that fidelity spectrum explicit config instead of a hidden
assumption. Each tier is a reference Meter implementation behind the same
MeterInterface hooks; the Admitter neither knows nor cares which tier produced
an estimate.

## The three tiers

### exact — tokenizer-exact hard upper bound (local/OSS endpoints)

For endpoints the user controls, `Estimate` computes a **hard admission
bound**: tokenize the full assembled input (skill prompt plus context bundle)
with the endpoint's own tokenizer, assume no KV/prefix-cache reuse, fresh
input, and maximum configured output. The bound is deliberately pessimistic —
it is an admission guarantee ("this step cannot exceed N tokens"), not a
prediction of typical cost. The tokenizer is an opaque user command invoked
host-side with the input text on stdin, emitting `{"tokens": n}` — faber
ships no tokenizer and learns no model family. `Actual` reports the same
unit from the endpoint's response usage when present, else the bound.

### reported — vendor usage blocks (metered APIs)

For endpoints that report usage in responses, `Estimate` returns a **zero
cost tagged with the tier's unit** — it cannot bound ahead of time, but the
zero claim keeps the meter participating in its budget's admission, so an
already-exhausted budget still rejects the step. `Actual` reads the usage
sidecar the box deposits next to its result record (the agent phase captures
the vendor usage block there) and maps its fields to unit-tagged costs. All
real enforcement in this tier is retrospective: budgets act as circuit
breakers, not pre-commitments.

### probe — opaque user probe (subscription endpoints, no usage API)

For endpoints with no usage accounting at all, the user may supply a probe: an
opaque command faber runs host-side before admission, emitting a saturation
reading `{"utilization": 0..1, "reset": epoch?}`. A typical user probe is a
cheap one-token canary request that reads the endpoint's rate-limit
utilization headers — but that is **user policy shipped in the user's config
layer, never a faber default**: faber ships no probe, pins no vendor headers,
and treats every reading as best-effort. Utilization at or above the
configured threshold ⇒ the estimate carries a saturation hint (`defer until
reset`); below ⇒ admit with no cost claim. A failing or malformed probe
degrades to admit-with-warning — a broken probe must not wedge a run the 429
floor can still protect.

## Tier selection

Tier assignment is declared per **endpoint class** — a user-chosen name for an
API surface (matching the vocabulary of the credential service names) — in a
metering config file supplied at run entry, mapping each class to a tier, a
unit, tier-specific settings (tokenizer command, max output, usage-field map,
probe command, threshold), and the templates it covers (default: all).
`orchestrator.yaml` is untouched: measurement policy is run policy, separate
from workflow structure, which is also what lets the same workflow run metered
in one environment and unmetered in another. No file, no `--budget` ⇒ the
no-op default.

Fidelity ordering is advice, not enforcement: exact where you can, reported
where the vendor allows, probe as a last resort — and beneath all three,
RateLimitDefer works with zero configuration.

Requirements implemented: Endpoint fidelity tiers.
