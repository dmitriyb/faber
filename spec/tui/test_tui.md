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
   absent under `…:20be39bb9bb7`, Home prints `images   4   4 MISSING`
   and a row per image, `merge   faber build   20be39bb9bb7`.

10. **Timeline cursor, letters, zoom, drill-down.** Rows are `implement`,
    `review-cycle@1`, `review-cycle@2` under `whole run`; `j` twice
    selects iteration 2 and marks its row; enter drills in: `review` is
    the only row, the breadcrumb reads `review-cycle 2/3`, the parent bar
    says `in task`, STEP reads `review-cycle 2/3 > review`, the footer
    names `-> in   <- up`; left comes back up with the
    cursor on iteration 2, right drills in again, esc comes up, and esc
    at the top leaves the run; at a 12-column scale the
    3-minute implement keeps its letter beside the review that follows;
    `+` and `-` zoom in and out and the legend says `zoom x2`.
11. **History as a distribution, in a named scope.** The store's `task`
    `review` attempts give n=1 (the failed one is not history), p50 = p90
    = p99 = 4m; a run's STEP shows `history   1 attempt   all runs of
    task   h to switch` with the three rows; on `implement`, which ran ok
    in both runs, `h` switches to `1 attempt   this run` and back, the
    cursor untouched; the preview rows carry `p50 … p90 …`.

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

17. **Timeline drills down.** Forty-five iterations each holding a
    nested `review-cycle`: enter on `cycles@21` shows `implement
    review-cycle@1 merge` on its own axis, enter on the nested iteration
    shows `review fix` with the path `cycles 21/45 > review-cycle 1/3`
    and the parent bar `in cycles@21`, a 24-row pane does not overflow,
    and backspace twice returns to the top with the cursor on row 21.

18. **Preflight.** Against the reference config, whose hooks and keys are
    not beside it, Home reads the checklist: `keys          MISSING
    3 of 3 roles` with `implementer   key file not found` and `merger` as
    its two rows and `+1 more`, `hooks         MISSING       5 slots in 3
    templates` with `fix/context   ./hooks/fetch-change missing` first,
    `credentials   MISSING       resolver missing, 1 service` with
    `resolver   ./hooks/get-token missing` beneath it. A copy with
    the files present, run from its own directory, two roles by registry
    (a fake key probe knows one) and one declared role no template uses,
    reads `keys          MISSING       1 of 4 roles, 1 unused` with
    `merger        not in the registry   faber add-key --role merger`,
    `hooks         ok            4 templates`, `credentials   ok
    resolver ./hooks/get-token, 1 service`. Enter on the `keys` line
    drills in: the crumb `PREFLIGHT > keys`, every role with its state and
    fix, `j` moves, `esc` comes back with the cursor on `keys`.
19. **Preview folds.** The task preview opens with `> PARAMS   2 params`,
    `> STRUCTURE   1 loop`, NEEDS opened by its own MISSING count showing
    the checklist narrowed to the task (`images   not checked   4 of 4`,
    `keys     MISSING   3 of 3 roles` with two rows), STEP open on
    `implement` with `v hooks   3 of 4 set   3 MISSING` listing all four
    (`postlude   none`), `> inputs/outputs   2 inputs   2 outputs`,
    `> history   2 attempts   p50 3m00s`; PARAMS' rows are not on screen.
    `tab` focuses the pane and the footer says `tab back to the flow`;
    enter opens PARAMS; `j` twice and enter fold NEEDS despite its problem;
    `tab` returns the cursor to the flow, where `j` moves again.

20. **One key vocabulary.** Left on Home focuses the project pane and
    right the runs; page keys clamp. On the preview, right focuses the
    details pane, esc returns to the flow before it leaves the preview,
    left returns too. On the tree, right opens an iteration and left
    closes it from inside, cursor on its row. `p` on the open settled run
    says `not live`, `R` on the open live run says `is live`. At 80
    columns the tree footer folds to two rows and still ends in `q back`.

21. **Runs table.** Thirty settled runs newer than the live one: the live
    run is first, a blank line under it, the newest settled run right
    after; the title reads `LIVE 1`; four page-downs clamp on the last row
    and it is on screen with the live block still at the top; `/failed`
    leaves the failed run alone and the title names the filter; esc clears
    the filter, a second esc quits.

22. **NEEDS lists the workflow's images.** With a probe that reports every
    image absent, the task preview's NEEDS reads `images   MISSING   4 of 4
    faber build` with `implement   faber build   20be39bb9bb7` and
    `review` beneath it; the epic preview lists its own templates' images.

23. **STEP shows the hooks' events.** The failed review's attempt carries
    an events file with a verified push, a failed checks read and one line
    that is not JSON: STEP reads `hooks   attempt 1   2 events   1
    malformed line`, then `fetch-change   push_verify   ref=work/x
    result=ok`, `checks_read   attempts=3  pr=17  result=failed` and
    `malformed  line 3: not a JSON object`; the now strip reads `next:
    resume`. A `halt` event appended with a `next` sentence puts that
    sentence after `next:`. The live run's implement, which no hook wrote
    about, has no hooks block.
24. **Preflight states.** With a probe that answers every tag with
    `ErrImageInputs: hooks.prelude not found`, Home's images line reads
    `not checked   4 of 4, a hook is missing or broken` and no FAIL appears; with a
    probe whose tags fail for another reason (`no faber-box binary to
    bake`) it reads `FAIL   4 of 4` with the reason on the rows, the
    template prefix dropped.
25. **STEP and OUTPUT scroll.** On the failed run with the failed review
    selected, `tab` focuses STEP (its title in reverse), `j` three times
    drops three rows from its top and the title reads `3 above`; `tab`
    again focuses OUTPUT, `k` walks back from the tail and the title says
    how many lines lie above; `esc` returns the focus to the list; moving
    the list's cursor resets both offsets; at 24 rows the footer still
    ends in `q back`.

## Edge cases

- Hooks are four fixed slots per template; the `hooks` class lists only
  a template's slot whose script is missing or broken, never the slots
  that are fine.
- A missing hook is one problem, not two: the hooks class names the slot
  and path, and the images line reads `not checked` with `a hook is
  missing` as its fact, never a second `FAIL` repeating the path; a tag
  failure of another kind (no faber-box to bake) is `FAIL` with its reason
  minus the template prefix.
- The checklist never scrolls: five classes, at most two problem rows
  each (one under 30 rows) and a `+k more`; everything beyond is the
  drill-down, which windows around its cursor like the runs table.
- Paths: a root config's hook, key and resolver paths are CWD-relative,
  so the preflight checks them against the working directory the view
  (and a run it spawns) has; an included file's are declarer-absolute and
  are shown relative to the config.
- Narrow terminals: the runs table folds its columns, the header is cut
  before the right-hand facts, and the footer drops the CLI equivalent
  rather than overflowing.
- A config that no longer loads: scenario 15.
- Output tails: controls and non-ASCII render as `?`; a zero-height pane
  reads nothing.
