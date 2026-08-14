# Mixed harness: one workflow, a harness per role

Wires an exact harness to an exact role: the `implement` box runs the goose
CLI against a self-hosted endpoint, the `review` box runs claude-code against
its vendor default — in one workflow, purely as config. faber itself knows
none of the vendor words in this example; every one of them arrives as user
data through three seams on the template (a template is a role's box):

- **the binary** — the template's package set (here via `overlay:`) plus
  `run.env.FABER_AGENT_CLI`;
- **the invocation dialect** — the `invoke_profiles:` entry the template's
  `invoke:` block selects. The `goose` profile moves the prompt to `-t` after
  a `run` subcommand, passes the skill as `--recipe` instead of a `/skill`
  prompt prefix, and drops the model/effort/budget flag pairs the engine
  would otherwise append (`model_flag: ""` etc.). A template with no
  `invoke:` gets the anonymous built-in default — exactly the invocation
  faber always emitted;
- **provider/endpoint knobs** — opaque `run.env` values the harness reads
  (`OPENAI_BASE_URL` pointing at a local endpoint, and friends). faber passes
  them through without interpreting them.

Check the profile's flags against the harness version your overlay actually
pins — the dialect is your config, so it tracks your toolset, not a faber
release.

```sh
faber validate --config examples/mixed-harness/orchestrator.yaml --emit-ir
faber build    --config examples/mixed-harness/orchestrator.yaml
faber run task --config examples/mixed-harness/orchestrator.yaml --param item=I-1
```

What to notice:

- `model:`/`effort:` stay mandatory on every template — they pin the
  cost/quality knobs in config. A profile whose harness has no such flags
  drops the *pairs* (empty flag values); the pinned values then reach the
  harness however it expects them (here, `GOOSE_MODEL` in the box env).
- Inline `invoke:` fields override a named profile field-by-field:
  `invoke: {profile: goose, budget_flag: --cost-cap}` would re-enable a
  budget pair without touching the library entry.
- The resolved profile is compiled into the IR (`--emit-ir` shows it under
  the template's `invoke` key), rides to the box as `FABER_INVOKE_PROFILE`,
  and changing it changes the IR hash — a resumed run refuses the drift
  instead of silently re-dialecting a step.
