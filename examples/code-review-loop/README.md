# Code-review loop: implement → (review; fix)* → merge

The production-shaped workflow: one work item is implemented, reviewed in a
bounded loop (max 3 iterations, settling early on `verdict == "approved"`),
and merged — each phase in its own sealed box under its own role identity.

```sh
faber validate --config examples/code-review-loop/orchestrator.yaml
faber build    --config examples/code-review-loop/orchestrator.yaml
faber run task --config examples/code-review-loop/orchestrator.yaml \
  --param repo=sandbox --param item=I-1
```

## What each piece buys you

- **Role isolation by construction.** `implement`/`fix` sign as the
  implementer key, `review` as the reviewer, `merge` as the merger — each box
  gets an ephemeral ssh-agent holding exactly that one key, torn down when the
  step ends. Role *enforcement* (fingerprint → allowed actions) belongs to
  your gateway, which is the only remote the box can reach, pinned by host key.
- **The loop is compile-time sugar.** `faber validate --emit-ir` shows it
  unrolled into three conditional review/fix pairs plus a selector that
  resolves post-loop references (`steps.review.verdict` in `merge`'s `when`)
  to the final executed iteration. Exhausting all three iterations fails the
  loop; `merge` is then skipped as dependency-failed.
- **An unfavorable verdict is not a failure.** `review` emitting
  `{verdict: changes}` is a successful step; the workflow's conditions react
  to it. Failures are crashes, schema violations, hook aborts — those
  fail-stop, run `on_failure` cleanup (`hooks/release-item`), and journal for
  `faber resume`.
- **No secret enters a box.** The agent-API credential is delegated in
  `proxy` mode: the box calls an unauthenticated on-network endpoint and your
  token-proxy injects auth. The `get-token` resolver runs host-side only.

## Adapting it

Everything domain-specific is in `hooks/` (plain executables; inputs arrive
as `FABER_INPUT_*`, context hooks write `CONTEXT.md` into `$FABER_BUNDLE_DIR`)
and in the companion services you run beside faber — the gateway, egress
proxy, and token proxy on the `agents-internal` network. See
[`docs/deployment.md`](../../docs/deployment.md) for that topology, including
a docker-compose sketch. Replace `keys/` placeholders with your real key
references and the pinned gateway host key before running.
