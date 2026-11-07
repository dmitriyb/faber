# Data flow: Box execution flow

What one agent step looks like from the host's docker run to the typed record
that threads onward. Everything between the two horizontal boundaries runs
inside the sealed container.

```
host: step spec (resolved template + bound inputs)
        │  box contract: FABER_* env (incl. FABER_SKILLS_LINK,
        │  FABER_SECRETS_STDIN); no host path mounted — faber-box, the
        │  hooks and /faber/skills are image content, /faber/result a
        │  tmpfs; binding fragments (network/remote/identity/credentials)
        │  composed by security, argv assembled by infra; file-mode
        │  tokens streamed on stdin
        ▼
──────────────── container start ───────────────────────────────
 0 preamble: chown the writables (result, bundle, tmp, home), drop to the run user
 1 skills link                 $HOME/<link> ──► /faber/skills (no-op if unset)
 2 env contract check          FABER_INPUT_*, FABER_SKILL, dirs
 3 stdin payload ──► /run/secrets/* (file mode)  then  /run/secrets/* ──► process env
 4 host-key policy             pinned │ tofu │ abort
 5 git clone gateway/<repo>    only when the repo input is bound
 6 signing config              ssh-add -L ──► git config key::<pub>
 7 context hook  ─┐
 8 prelude hook  ─┴──►  bundle dir: CONTEXT.md + bundle.env + sidecars
 9 agent          prompt = /<skill> + CONTEXT.md [+ extra instruction]
        │         side-effects: repo mutations, pushed via the gateway
        ▼         output.json into the result dir
10 extract ──► validate (output schema) ──► verify (declared effects)
        │
        └──► result.json (one attempt record)      [any failed phase:
                                                    handoff.json + failed
                                                    result.json, exit 1]
   ship: result/** (result.json last), sessions/**, end ──► stdout frames
   (events.jsonl lines went out as event frames while 7–10 ran;
    everything human — box log, hook and agent output — went to stderr)
──────────────── container exit ────────────────────────────────
        │  stdout ──► demuxer ──► <attempt>/result/**, <attempt>/sessions/**
        │  stderr ──► <attempt>/output.log (live, as it streamed)
        ▼
host: ExtractResult ──► re-validate ──► failure.Result
        ├──► threading (declared output fields to downstream slots)
        ├──► journal (keyed step-id, input-hash)
        └──► meter actual(result)
```

## Shapes at each boundary

| Boundary | Shape | Contract |
|----------|-------|----------|
| host -> box | env vars (+ stdin in file mode); no host path | typed inputs as `FABER_INPUT_<SLOT>`; no secret ever in the docker `-e` argv or a host file — file-mode tokens arrive as a JSON payload on the container's stdin |
| setup -> hooks | the box environment | hooks see inputs, handles, workspace cwd; nothing else |
| hooks -> agent | context bundle | `CONTEXT.md` mandatory (or synthesized); `bundle.env` values opaque, `BRANCH` = declared side-effect |
| agent -> extractor | `output.json` | declared output fields only; absence triggers the fallback |
| box -> host | frames on the container's stdout: `event` lines live, `result/**` and `sessions/**` at the end, `result.json` last, then `end` | exactly one record per attempt: `{status, payload\|error, timing, attempt}`; names contained under two roots, bodies bounded, a truncated frame is an absent file |
| host -> downstream | `failure.Result` | schema re-validated before threading, journaling, metering |

## Error paths

Every failure converges on one funnel: any phase error writes `handoff.json`
plus a failed `result.json` and exits nonzero — hook exit, missing bundle,
agent crash, schema violation, and unverified side-effect differ only in the
record's `error.reason` and handoff contents. There are exactly two shapes the
host can observe after exit: one valid record, or no readable record (the
sequencer itself died, or the stream broke before its last frame), which the
host converts into a synthesized `box-vanished` failure. Two records, zero
records, or a half-written record cannot occur (atomic rename in the box,
`result.json` sent last, temporary name and rename on the host).

## Who runs it

The pipeline module's scheduler, once per ready agent node per attempt: it
resolves the step's inputs from prior results, asks security for the binding
set and infra for the container run, then calls `ExtractResult` and hands the
record to failure (journal), metering (actual cost), and its own threading.
The box never knows it is part of a DAG — its whole world is the env contract
in and the result contract out, which is what makes a single step
reconstructable for interactive recovery.
