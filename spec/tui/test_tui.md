# Test section: TUI tests

Unit tests over a seeded store and the reference config; no terminal, no
docker. The screens are rendered into frames and compared colour-stripped.

## Fixtures

- A temp store with two `task` runs against `config/testdata/reference.yaml`:
  one failed (implement ok; review-cycle@1 review changes → fix pushed;
  @2 review failed with a handoff; the rest not reached; run-end settled,
  1 failed) with an `output.log` for the failed review; one live (implement
  ok, review@1 started four minutes ago, the run lock held by the test).
- A fixed clock.
- Goldens under `testdata/`: `home`, `run-tree`, `run-flow`,
  `run-timeline`, `run-live` at 221×51 and 80×24, and `workflow-flow`,
  `workflow-tree` at 221×51 (`UPDATE_GOLDEN=1` regenerates). The four design mock-ups the screens were settled against
  live in the proposal's history; the goldens are the renderer's own.

## Scenarios

1. **Golden screens.** Home and the three Run views on the failed run, and
   the tree on the live run, at both sizes, match their goldens.
2. **Live derivation.** The live run lists first as LIVE, the failed one as
   FAILED; the live run's running step is review-cycle@1/review; the now
   strip reads `review-cycle 1/3 > review   running  4m00s   usually 4m00s`;
   releasing the lock drops LIVE on the next refresh.
3. **Tree folding and keys.** Rows read `implement | ok`, `review-cycle |
   FAIL | 2 of up to 3`, `@1 | ok | review . fix`, `@2 | FAIL` expanded with
   `review | FAIL | agent-failed` and `fix | not reached | after review`,
   `@3 | not reached`, `merge | not reached`; `n` selects the failed review;
   enter on iteration 1 adds its two rows; `/fix` leaves only fix rows; `q`
   returns home.
4. **Honest actions.** Quitting the run with the failed review selected
   sets the quit command to `faber resume <id> --interactive
   task/review-cycle@2/review`; `p` on the settled run reports "not live"
   and writes no marker.
5. **Colour is additive.** The colour-stripped coloured frame equals the
   plain render; `FAIL`, `STEPS`, and identifiers carry their slots; no
   256-colour sequence appears.
6. **Key decoder.** `j`, CSI up, CR, CSI 6~, a lone ESC, `q` decode to
   `j up enter pgdn esc q` — the key after a lone escape is not eaten.
7. **Flow rows.** On the failed run the flow reads `implement ok`, the
   `review-cycle` frame at `2 of up to 3` with `until review.verdict ==
   "approved"`, inside it `review FAIL` and `fix` with `if review.verdict
   == "changes"` not reached, the closing edge, then `merge` with its
   guard; `j` from the top lands on `review` (frame edges are skipped) and
   STEP follows the flow cursor.

8. **Workflow preview.** Tab moves the Home focus to the workflows pane
   (and the runs pane keeps only a dim marker, so one row is highlighted);
   enter on `task` opens the preview; its flow and tree frames at 221×51
   match their goldens; rows carry `review-cycle||up to 3 iterations` with the
   body steps once and no status words; `j` moves the flow cursor; `q`
   returns home.
9. **Missing image names its tag.** With a probe that reports every image
   absent under `…:20be39bb9bb7`, Home prints `MISSING  faber build
   20be39bb9bb7`.

10. **Timeline cursor, letters, zoom.** Rows are `implement`,
    `review-cycle@1`, `review-cycle@2`; `j` twice selects iteration 2,
    marks its row and expands it below; at a 12-column scale the
    3-minute implement keeps its letter beside the review that follows;
    `+` and `-` zoom in and out and the legend says `zoom x2`.
11. **History as a distribution.** The store's `review` attempts give
    n=2, p50 3m, p90 and p99 4m; a run's STEP shows `p50 … p90 … p99 …
    over 2 attempts in the store`; the preview rows carry `p50 … p90 …`.

12. **Spawn is a faber child.** With `Options.Executable` pointing at a
    script, `r` runs `faber run task --param repo=x --config <abs>
    --progress json` detached; the script sees exactly those arguments and
    its output lands in the log the status line names.
13. **Resume refused on a live run.** `R` on the LIVE row sets a status
    naming the run and spawns nothing.
14. **Quit keys and tiny screens.** Bytes 0x03 and 0x04 end the view like
    `q`; every view renders at 80×9, 40×12 and 221×51 with exactly that
    many rows; an empty store renders Home with `no runs in the store yet`
    and at 80×9.
15. **A run without its config.** A third run whose header names a config
    that is gone opens with `Err` set and no IR: the tree still lists its
    steps, the strip carries `config: …`, and the flow says `nothing to
    show`.
16. **Replay cache by stat.** A quiet tick leaves the cache version
    unchanged; appending a record to a journal bumps it, the history
    follows, and the new step's attempt appears in it.

17. **Timeline keeps room for the selection.** Forty-five iterations of
    three steps on a 24-row pane: the selected row is visible, its three
    attempts are listed below with `by attempt (3)`, nothing overflows;
    enter hides the block and leaves the view on the timeline.

## Edge cases

- Narrow terminals: the runs table folds its columns, the header is cut
  before the right-hand facts, and the footer drops the CLI equivalent
  rather than overflowing.
- A config that no longer loads: scenario 15.
- Output tails: controls and non-ASCII render as `?`; a zero-height pane
  reads nothing.
