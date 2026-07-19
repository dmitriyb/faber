# ResultContract — one record per step attempt, four consumers

## What it is

The single structured record every step attempt produces, and the module's
foundational type. Failure is a record, not an absence: whether the box ran to
a clean result, the agent crashed, a hook refused to start it, or the container
never launched, the engine holds exactly one record of the same shape.

```
{status: ok|failed, payload (schema-typed) | error {reason, detail, handoff},
 timing, attempt}
```

Exactly one of `payload`/`error` is present, discriminated by `status`. The
record is written by the box to the mounted result directory as `result.json`;
the engine writes a minimal fallback if the agent succeeded but wrote none
(the proven harness does the same). The payload is schema-validated against
the template's declared output — by the agent module's ResultExtractor, before
anything downstream sees it — so every consumer of an `ok` record may trust
the payload's shape without re-checking.

## One contract, four consumers

The same record is simultaneously:

1. **The threading payload.** `${steps.X.field}` bindings read `payload.field`
   from X's record — the data edges of the DAG carry these bytes and nothing
   else.
2. **The failure signal.** The Scheduler and FailurePolicy branch only on
   `status`; they never inspect the payload.
3. **The journal entry.** The record, keyed `(step-id, input-hash)`, is what
   the Journal appends — durability and threading share one encoding.
4. **The metering input.** `Meter.Actual(result)` reads the record for cost
   accounting, and the 429-defer floor reads a failed record's error to
   extract a rate-limit reset time.

One contract keeps these four views from drifting: there is no side channel by
which a step can "succeed for threading but fail for the journal".

## Status semantics

`ok` means the step completed its contract and produced a schema-valid
payload. An unfavorable-but-valid output is `ok`: a review step emitting
`{verdict: changes}` did its job perfectly — workflow conditions (`when:`,
`until:`) react to the verdict; failure semantics do not. `failed` is reserved
for the step not producing what its contract promises: hook failure, agent
crash, schema violation, gateway rejection of the push, loop exhaustion (the
selector's bound reached without settling).

## The error record and the handoff pointer

`error` carries a stable machine `reason` — a small vocabulary per producer.
The failure module's own words: env-contract, hook, agent, result-schema,
verify, launch, canceled, loop-exhausted, source-contract. The box contract
contributes its per-phase words (secrets, host-key-policy, clone-failed,
signing, hook-failed, bundle-missing, bundle-malformed, agent-failed,
output-schema, missing-output, side-effect-unverified, result-write) plus the
host-synthesized box-vanished and contract-version; the pipeline contributes
its admission words (budget, admission, condition, expansion) and the
reserved skip/annotation encodings (never trusted from an executed record).
Box-authored reasons colliding with the reserved vocabulary are namespaced
`box:` at the extract boundary. Beside `reason` sits a `detail` — human text by default, machine-readable JSON by
per-reason convention (a `rate-limit` reason carries the reset epoch the
metering module's defer floor reads) — and an optional `handoff` — a pointer,
resolvable under the run directory, to preserved diagnostic state. This
generalizes the proven harness's failure handoff: on agent failure the box
copies its harness state (context bundle, hook outputs, environment summary)
into a handoff directory and writes a reason file; the engine preserves that
directory past container teardown and records its path. Recovery tooling —
notably the interactive recovery mode — resolves the pointer to show the
operator exactly what the failed box saw.

## Timing and attempt metadata

`timing` records start, finish, and duration for the attempt. `attempt` is the
1-based attempt number; when retries were exhausted or eventually succeeded,
the final record additionally carries the prior attempts' history (status,
timing, error reason per attempt) so the journal and run report show the whole
story in one record. FailurePolicy owns attempt accounting; this component
only defines where it lands.

Requirement implemented: Structured step result.
