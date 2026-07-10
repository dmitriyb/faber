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
most a warning and never affects the attempt's status.

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
agent can forge its own record, and re-validation bounds the damage to
mis-shaped *data* — it can never touch security, which lives entirely in the
bindings and the user's gate service. If `result.json` is missing or
unparseable after exit (the sequencer itself was killed), the host
synthesizes a failed record with reason `box-vanished`; no path yields zero
records, and no path yields two.

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
