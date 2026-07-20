# ResultExtractor — the container-boundary result

## What it is

The final phase of the box plus a thin host-side boundary read. Its guarantee:
when the container exits, the mounted result directory contains exactly one
attempt record, `result.json`, in the failure module's ResultContract shape —
already schema-validated against the template's declared output and checked
against the bundle's declared side-effects. Failure is a record, not an
absence: every exit path of the box, including every fail-stop, lands here.

## Extraction and validation

The skill writes its declared output fields as a JSON object to
`$FABER_RESULT_DIR/output.json`. After a successful agent phase:

- **Present** → parse and validate against the schema in
  `FABER_OUTPUT_SCHEMA`: every required field present, types exact
  (string/int/bool/object, no coercion), enum membership respected. Extra
  undeclared fields are preserved in the record but are never threaded — only
  declared fields exist to downstream wiring, which the WiringChecker already
  proved. All violations are collected; any violation makes the attempt
  `failed` with reason `output-schema`.
- **Absent** → the fallback: an engine-written record with an empty payload
  and a `fallback: true` marker. Validation then applies normally, so a
  template with required outputs converts the fallback into a `missing-output`
  failure, while a template with no required outputs tolerates a quiet agent.
  Either way there is a record — an agent that says nothing does not produce
  an absent step.

Beside `output.json`, the skill may deposit optional sidecars. The one named
convention: `usage.json`, a vendor usage block the metering module's reported
tier reads for actual-cost accounting. Sidecars are advisory — absence is at
most a warning and never affects the attempt's status — but their **content
is box-authored bytes on the budget path**, so the host treats it exactly as
untrusted input: the read is size-bounded, a malformed or oversize sidecar is
logged and ignored (silently dropping it would quietly disable the reported
tier), values are clamped non-negative before they reach any meter, and the
ledger's own arithmetic clamps and saturates besides — a hostile sidecar can
neither refund a budget nor wrap the spent counter. A configured usage field
absent from the sidecar is a logged warning (a renamed vendor field must not
silently read as zero).

The record itself (owned by failure's ResultContract) is
`{status: ok|failed, payload | error {reason, detail, handoff}, timing,
attempt}`; `attempt` comes from `FABER_ATTEMPT`, timing from the sequencer's
phase clocks, and the handoff pointer references the fail-stop record when one
exists.

## Declared side-effect verification

`bundle.env` entries are postconditions, not annotations. The one first-pass
convention: `BRANCH=<name>` with a bound `repo` means "after this step, that
branch exists on the gateway" — verified via
`git ls-remote --exit-code origin refs/heads/<name>`. Verification runs
*in-box* (only the box can reach the gateway) and after extraction: a payload
that validates perfectly but claims a push the gateway never accepted becomes
`failed` with reason `side-effect-unverified`. This is the generic form of the
proven harness's outcome check — the payload is the agent's claim; the
gateway's state is the evidence.

## The host boundary

After the container exits, the host re-parses `result.json` from the mounted
directory and re-validates the payload schema before the record reaches
threading, the journal, or the meter. The box is untrusted: a compromised
agent can forge its own record, and the boundary bounds the damage field by
field:

- **Reads are size-bounded.** Every container-written file the host reads
  (`result.json`, `handoff.json`, `usage.json`) has a hard byte bound;
  oversize is never a silent truncation. For the load-bearing records
  (`result.json`, `handoff.json`) oversize is a hard read error; for the
  advisory `usage.json` sidecar it is logged and treated as no usage (per the
  sidecar's advisory-only contract above).
- **The contract stamp is asserted.** A record whose `contract` version is
  not this host's (including the absent stamp of a foreign writer) fails
  with reason `contract-version` — a `FABER_BOX_BIN` misconfiguration
  detector, never interpreted on guessed semantics.
- **The payload is re-validated** against the declared output schema and the
  unthreaded set recomputed host-side; mis-shaped data becomes a failed
  record and never threads.
- **The handoff pointer is containment-checked.** The box-authored pointer
  must resolve strictly under the attempt's result directory (the same
  discipline the skill stager applies to names); an escaping pointer is
  dropped with a note in the record's detail — its parent is what the
  interactive mode later bind-mounts into the operator's container, and the
  recovery-mode resolver re-checks containment under the run dir when it
  consumes the journaled pointer.
- **Box-authored text is terminal-sanitized at render.** Reasons, details,
  and payload values are stripped of control bytes on the report's human
  Text path, so an escape sequence cannot re-style or forge the operator's
  screen (the JSON path is encoding-escaped by construction).
- **Reserved reasons are namespaced** (`box:` prefix) so a record can never
  masquerade as a scheduler skip or annotation.

Re-validation bounds the damage to mis-shaped *data* — it can never touch
security, which lives entirely in the bindings and the user's gate service.
If `result.json` is missing or unparseable after exit (the sequencer itself
was killed), the host synthesizes a failed record with reason `box-vanished`
— and when the container actuation itself failed (daemon unreachable, image
missing), the synthesized record carries the actuation error and a bounded
output tail, so the true cause is in the journal rather than at debug level.
No path yields zero records, and no path yields two.

## Deferred seam: non-JSON artifacts

First pass, a step's durable outputs are exactly two: the typed JSON payload
and repo state behind the gateway. Deliverables that fit neither — large
files, research outputs, artifacts that belong to no git repo — have no home
yet. Reserved: an artifact store or a pass-by-reference convention in the
result contract (a typed reference field naming an artifact the engine moves
or mounts). The record shape does not block this: references would be ordinary
typed payload fields whose values the engine learns to dereference. Until
then, a workflow needing such outputs must route them through a repo behind
the gateway.

Requirements implemented: Structured result extraction; Deferred: non-JSON
artifacts.
