# Change Proposal: Loop cap lever — expected count, boundary stop, cap-tolerant resume

## Context

A loop's `max` is a plain integer unrolled at desugar time into
`<loop>@1..@N`, and the resume guard refuses an IR-hash mismatch. Together
they make the cap the one knob an operator cannot turn mid-run: an epic
with 50 beads under `cycles: {max: 45}` runs 45 cycles, exhausts as a
failure, and the only way forward is `--fresh`, which re-does every landed
cycle. The user's own README says "bump max and `faber resume`" — the
guard as written refuses exactly that.

The operator usually knows the answer before the run starts: the number of
ready tasks is a query against their tracker. Faber must not learn what a
task is, but it can run an opaque command and compare a number with a cap,
exactly as `generate` runs an opaque command and fans out over its items.

Two more facts shape the design. Runs are unattended, so the right reaction
to "the cap is insufficient" is a stop with a clear message, never a prompt.
And the cap is not a field in the IR: it is baked into the node count, the
selector's candidate list, and the iteration edges, so "exclude the cap from
the hash" has to mean hashing a cap-normalized projection of the IR.

## Proposed change

- **`expect:` on a loop.** `loop:` gains an optional `expect: <source name>`,
  naming a `sources.<name>` command of the enclosing workflow — the same
  opaque `{command, args}` shape generate uses, args resolved from the loop's
  scope (params at the top level, the instance's params inside a generate
  instance). It prints `{"count": N}` on stdout: the number of iterations the
  loop is expected to need. The mapping from the user's tasks to iterations
  is the command's business; faber sees a count. Validation refuses an
  `expect` that names no declared source. Desugaring records, on the IR root,
  a loop table entry `{id, max, expect: {command, args}}` for every loop that
  declares one — omitted when none does, so the IR stays byte-identical.
- **Boundary stop.** The scheduler runs the loop's expect command at the
  loop's entry — before its first iteration's first node dispatches — and
  again at every iteration boundary, before iteration i+1's first node
  dispatches. When `count > max`, the run pauses: nothing further dispatches,
  in-flight steps settle and journal (the existing cooperative drain), and
  the run ends `paused` with exit 4 and the detail
  `<loop>: expected N, cap M; raise the cap and resume`. For a loop that is
  the workflow's first step this is a stop before anything runs. The
  command's failure or malformed output fails the loop's current iteration
  nodes with the source-contract reason, exactly like a generate source. A
  count at or under the cap is silent; the count is emitted on the progress
  stream (`expect  cycles  12 of up to 45`) so the renderer can say
  `cycles 3/12 (cap 45)` later.
- **Cap-tolerant resume.** The journal header gains `ir_hash_capfree`: the
  hash of the IR desugared with every loop's `max` forced to 1 — the same
  pipeline, so anything else that differs (a template, a guard, an edge)
  still changes it. Resume compares that key when the header carries it,
  and additionally refuses when any loop's new cap is below the highest
  iteration the journal shows executed for it (a selector would otherwise
  adopt an earlier iteration's result as the loop's value). Journals from
  before the field keep the full-hash rule. Raising a cap is therefore a
  config edit plus `faber resume`: iteration node ids are stable, so the
  landed iterations are journal hits; the loop reached its cap only by
  exhausting, which left every dependent as a hashless skip record, so the
  tail re-dispatches once the extended loop settles. `--fresh` escapes every
  guard as before.
- **Out of scope.** Lowering a cap below executed work (refused, not
  migrated); an interactive adjust prompt; estimating anything faber does
  not have a command for.

## Impact expectation

- **config**: `LoopDef.Expect`, validation against `sources`, the IR root
  loop table, `HashIRCapFree` (desugar with caps forced to 1 and hash);
  `JournalHeader.IRHashCapFree`; the resume command's early guard mirrors
  the store's rule; docs.
- **pipeline**: the expect gate at loop entry and iteration boundaries
  (host-side command through the existing data-source runner), the pause
  with its reason, the `expect` progress event and its rendering.
- **failure**: the header field, written fresh and carried on resume; the
  resume guard's cap-free comparison and the executed-iteration floor.
