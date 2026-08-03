# Batch fan-out: generate over a data source

One `rollout` run migrates every module a data-source command reports,
dependency-aware: `pkg/db` and `pkg/queue` migrate in parallel, `pkg/api`
starts only after both settle.

```sh
faber validate --config examples/batch-fanout/orchestrator.yaml
faber run rollout --config examples/batch-fanout/orchestrator.yaml --param repo=monorepo
```

This workflow is gated (it configures a `remote:`), so each role needs a
registered committer email before its boxes can run — boxes sign their
commits, the email must be one your forge can tie to the key's account, and
faber refuses to invent one:

```sh
faber add-key --role implementer --fingerprint SHA256:… --git-email you@example.com
```

The moving part is `hooks/list-modules`, an opaque command emitting

```json
{"items": [{"id": "pkg/api", "deps": ["pkg/db", "pkg/queue"]}]}
```

Expansion happens at run time: faber validates the item shape, instantiates
the (already validate-proven) `migrate` workflow once per item under
`rollout/migrations[<id>]/...` step ids, binds `${item.id}` into its params,
and splices the instances into the running DAG with edges derived from
`deps`. The run report rolls results up per item. A failed item fail-stops
its own dependents (here: `pkg/api` if either dependency fails) while
independent items continue; `faber resume` re-runs only what didn't settle.

First-pass semantics to know: an empty item set is a no-op run, malformed
data-source output fails the generate node before anything launches, and item
ids may not contain `[` or `]`.
