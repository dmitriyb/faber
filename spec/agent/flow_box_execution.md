# Data flow: Box execution flow

What one agent step looks like from the host's docker run to the typed record
that threads onward. Everything between the two horizontal boundaries runs
inside the sealed container.

```
host: step spec (resolved template + bound inputs)
        │  box contract: FABER_* env, result-dir mount, hook mounts,
        │  faber-box mount; binding fragments (network/remote/identity/
        │  credentials) composed by security, argv assembled by infra
        ▼
──────────────── container start ───────────────────────────────
 1 env contract check          FABER_INPUT_*, FABER_SKILL, dirs
 2 /run/secrets/*  ──►  process env (uppercased basenames)
 3 host-key policy             pinned │ tofu │ abort
 4 git clone gateway/<repo>    only when the repo input is bound
 5 signing config              ssh-add -L ──► git config key::<pub>
 6 context hook  ─┐
 7 prelude hook  ─┴──►  bundle dir: CONTEXT.md + bundle.env + sidecars
 8 agent          prompt = /<skill> + CONTEXT.md [+ extra instruction]
        │         side-effects: repo mutations, pushed via the gateway
        ▼         output.json into the result dir
 9 extract ──► validate (output schema) ──► verify (declared effects)
        │
        └──► result.json (one attempt record)      [any failed phase:
                                                    handoff.json + failed
                                                    result.json, exit 1]
──────────────── container exit ────────────────────────────────
        │
        ▼
host: ExtractResult ──► re-validate ──► failure.Result
        ├──► threading (declared output fields to downstream slots)
        ├──► journal (keyed step-id, input-hash)
        └──► meter actual(result)
```

## Shapes at each boundary

| Boundary | Shape | Contract |
|----------|-------|----------|
| host -> box | env vars + read-only mounts | typed inputs as `FABER_INPUT_<SLOT>`; no secret ever in the docker `-e` argv |
| setup -> hooks | the box environment | hooks see inputs, handles, workspace cwd; nothing else |
| hooks -> agent | context bundle | `CONTEXT.md` mandatory (or synthesized); `bundle.env` values opaque, `BRANCH` = declared side-effect |
| agent -> extractor | `output.json` | declared output fields only; absence triggers the fallback |
| box -> host | `result.json` in the mounted dir | exactly one record per attempt: `{status, payload\|error, timing, attempt}` |
| host -> downstream | `failure.Result` | schema re-validated before threading, journaling, metering |

## Error paths

Every failure converges on one funnel: any phase error writes `handoff.json`
plus a failed `result.json` and exits nonzero — hook exit, missing bundle,
agent crash, schema violation, and unverified side-effect differ only in the
record's `error.reason` and handoff contents. There are exactly two shapes the
host can observe after exit: one valid record, or no readable record (the
sequencer itself died), which the host converts into a synthesized
`box-vanished` failure. Two records, zero records, or a half-written record
cannot occur (atomic rename, single writer).

## Who runs it

The pipeline module's scheduler, once per ready agent node per attempt: it
resolves the step's inputs from prior results, asks security for the binding
set and infra for the container run, then calls `ExtractResult` and hands the
record to failure (journal), metering (actual cost), and its own threading.
The box never knows it is part of a DAG — its whole world is the env contract
in and the result contract out, which is what makes a single step
reconstructable for interactive recovery.
