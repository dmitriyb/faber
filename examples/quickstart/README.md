# Quickstart: one box, one typed output

The smallest end-to-end faber workflow: a single `summarize` step that takes a
`topic` param and must produce `{summary, confidence}` matching the declared
output schema before anything downstream could use it.

```sh
cd examples/quickstart
faber validate --config orchestrator.yaml --emit-ir
faber build    --config orchestrator.yaml
faber run brief --config orchestrator.yaml --param topic="what our retry policy does"
```

Before `faber run`, wire the credential once:

1. Create a token on the host with `claude setup-token`; it prints the token.
2. Put it in your secret store, for example `pass insert faber/claude_code_oauth_token`.
3. In `hooks/get-token`, replace the last two lines with the command that prints
   it, for example `exec pass show "faber/$1"`. The contract is the token on
   stdout, nothing else, exit 0.
4. Check it host-side: `./hooks/get-token claude_code_oauth_token | wc -c`
   prints a non-zero count.

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
  first run; production configs pin a network + proxy (see `docs/deploy.md`).
- `hooks/get-token` is a stub that exits 1 until you point it at your secret
  store; `faber run` resolves the credential through it before the box starts,
  `validate` and `build` never call it. The token lands in the box as
  `CLAUDE_CODE_OAUTH_TOKEN` (file mode, tmpfs, gone with the container).
- The build pins nixpkgs (`pin:`) and takes `claude-code` from
  `nix/overlay.nix`, a pinned Anthropic release; paths are read relative to the working directory, which
  is why the commands above start with `cd`.
