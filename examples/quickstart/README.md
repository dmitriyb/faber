# Quickstart: one box, one typed output

The smallest end-to-end faber workflow: a single `summarize` step that takes a
`topic` param and must produce `{summary, confidence}` matching the declared
output schema before anything downstream could use it.

```sh
faber validate --config examples/quickstart/orchestrator.yaml --emit-ir
faber build    --config examples/quickstart/orchestrator.yaml
faber run brief --config examples/quickstart/orchestrator.yaml --param topic="what our retry policy does"
```

What to notice:

- The template is hook-less: faber synthesizes the context bundle from the
  step's typed inputs. Add a `hooks: {context: ...}` script when the agent
  needs more than the raw inputs.
- `FABER_AGENT_CLI` names the agent binary the box invokes headlessly. There
  is no default — the agent is your policy, not faber's.
- The output schema is enforced at the container boundary: a missing
  `confidence` or a value outside the enum fails the step with a structured
  record, it does not thread garbage downstream.
- There is no `network:` section, so this box has open egress. Fine for a
  first run; production configs pin a network + proxy (see the other examples).
