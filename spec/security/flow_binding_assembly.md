# Data flow: Binding assembly flow

The per-step path from resolved config to the security slice of a `docker
run` argv, plus the hooks that bracket the container's life.

```
resolved step (IR node + params)
  network def ─ remote def ─ identity def ─ service defs ─ runtime knob
        │            │             │              │             │
        ▼            ▼             ▼              ▼             │
  NetworkBinding RemoteBinding IdentityBinding CredentialBroker│
   (preflight:    (read host    (resolve key,   (get_token     │
    net exists)    key line)     spawn agent,    host-side →    │
        │            │           load 1 key)     Secrets map)  │
        └────────────┴──────┬──────┴──────────────┴────────────┘
                            ▼   fixed order: network, remote,
                     BindingSet.Prepare   identity, credentials, runtime
                            │   (encode Secrets → SecretsStdin JSON, once)
              Assembled{Args, SecretsStdin, Attach, Teardown}
                            │
                            ▼
              infra.ContainerRunner (argv fragment spliced verbatim;
                            │         SecretsStdin streamed on stdin, -i;
                            │         Attach started beside the container:
                            │         the identity relay over docker exec,
                            │         cancelled when the container ends)
                            │  run, wait, capture
                            ▼
              Assembled.Teardown  ── reverse order, always
                                     (identity: nothing per attempt — the
                                      relay dies with the box, role agents
                                      stop at Close; file mode: nothing —
                                      tmpfs dies with the container)
```

## Shapes at each boundary

| Boundary | Shape | Contract |
|----------|-------|----------|
| step -> Binding | `StepSpec` | resolved values only; bindings never read YAML or the IR |
| Binding -> BindingSet | `Contribution{Args, Attach, Teardown}` | args internally ordered; attach runs for the container's lifetime or nil; teardown idempotent-once or nil |
| resolver -> broker | `Secret` on stdout | opaque, redacted formatting; non-zero exit = step failure |
| broker -> BindingSet | `Contribution.Secrets` (name→Secret) | file-mode tokens only; encoded once into `Assembled.SecretsStdin` |
| BindingSet -> ContainerRunner | `Assembled{Args, SecretsStdin, Attach, Teardown}` | fragment consumed verbatim; `SecretsStdin` streamed on stdin; `Attach` started beside the container and cancelled when it ends; the only docker-run assembly point is infra's |
| teardown -> caller | joined error or nil | every hook attempted; detached context so cancellation cannot skip it |

## Error paths

Assembly is fail-fast across bindings but complete within teardown: the
first `Prepare` error stops assembly, unwinds already-prepared bindings in
reverse, and surfaces as a structured step failure (failure module's
record) naming the binding — no container was launched, so there is
nothing else to clean. After a launched container exits (any status, or
kill-on-cancel), teardown runs exactly once; its errors are reported on
the step but do not overwrite the step's own result.

## Who runs it

The pipeline executor invokes the flow once per step *attempt*: retry
discards the previous `Assembled` entirely and re-prepares — a fresh relay
on the role's agent, fresh resolver call, fresh secrets payload — so
between-attempt cleanup can assume nothing survives an attempt. Nothing in
this flow is shared across steps but the run-scoped role agents; parallel
steps assemble concurrently against disjoint scratch directories and each
attaches to its own container. `faber validate` does not run this flow (it is
run-time by nature) but the loader has already proven the config slices
well-formed: exclusive host-key modes, known credential modes, declared
identities.
