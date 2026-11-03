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
- Goldens under `testdata/`: `home`, `run-tree`, `run-graph`,
  `run-timeline`, `run-live` at 221×51 and 80×24 (`UPDATE_GOLDEN=1`
  regenerates). The four design mock-ups the screens were settled against
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
6. **Key decoder.** `j`, CSI up, CR, CSI 6~, `q` decode to `j up enter
   pgdn q`.
7. **Graph layout.** implement, review-cycle, merge land in columns 0, 1,
   2 (the selector folded into its loop); no iteration node survives the
   collapse.

## Edge cases

- Narrow terminals: the runs table folds its columns and the footer drops
  the CLI equivalent rather than overflowing.
- A config that no longer loads: the run still renders from the journal
  with an error line, no IR.
