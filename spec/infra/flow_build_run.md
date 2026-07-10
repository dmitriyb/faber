# Data flow: Template to image to container

The actuation pipeline: how a template's declared toolset becomes a running
step container and, eventually, an exit code.

```
TemplateDef.build {packages, overlay}          (config module, validated)
        │  render proof expression, nix eval --json
        ▼
resolution proof (map[name]bool)               ImageBuilder.ProvePackages   [faber validate]
        │  toolset hash = pin rev + sorted packages + overlay bytes
        ▼
image tag "faber/<template>:<hash>"            ImageBuilder.ImageTag
        │  skip if docker image-exists; else render image.nix,
        │  nix build --json → tarball out path
        ▼
layered image tarball                          NixClient.Build              [faber build]
        │  docker load; loaded tag == computed tag asserted
        ▼
tagged image in the daemon                     DockerClient.Load
        │  + BindingSet argv fragment (security)
        │  + engine mounts (result dir rw, hooks ro) and env
        ▼
RunSpec → docker run argv (fixed order)        ContainerRunner.buildArgs    [faber run, per step]
        │  start, stream to bounded buffer, wait; kill on ctx cancel
        ▼
RunResult {exit code, output tail, timing}     ContainerRunner.Run
        │
        └──► agent module (result.json extraction) / failure module (record)
```

## Shapes at each boundary

| Boundary | Shape | Contract |
|----------|-------|----------|
| BuildDef -> proof | `map[string]bool` from one `nix eval --json` | no builds, no downloads beyond the pinned source; false entries become field-path errors |
| BuildDef -> tag | `faber/<name>:<12-hex>` | pure function of pin + sorted packages + overlay content; computable without nix |
| expression -> tarball | store out path from `nix build --json` | image content is a pure function of the toolset; no repo, no secrets |
| tarball -> daemon | tag string from `docker load` | must equal the computed tag, else the build fails loudly |
| bindings -> argv | ordered `[]string` fragment | spliced verbatim between engine env and image; never parsed by infra |
| argv -> container | one `docker run --rm --name <step>` | one container per attempt; no socket mount, no undeclared host mounts |
| container -> caller | `RunResult` | exit code is data, not error; output capped; result.json is the real artifact |

## Caching and identity

The image tag is the flow's identity fixpoint. Validate computes it without
building; build uses it to skip work already in the daemon; run embeds it in
the step's journal input-hash so a resumed run refuses to silently reuse a
step result produced by a different toolset. The same hash inputs that make
the tag deterministic (pin, sorted packages, overlay bytes) are what make
"rebuild" a meaningful event rather than a timestamp accident.

## Who runs which segment

- `faber validate`: only the proof segment — every template's packages proven
  resolvable, nothing built or loaded.
- `faber build [--template name]`: proof through docker load for the named (or
  every) template; idempotent by the image-exists skip.
- `faber run`: ensures the step's image exists (building on demand if not),
  then the RunSpec segment once per step attempt. The scheduler may run many
  RunSpec segments concurrently; the build segment for one tag is serialized
  behind a per-tag lock so concurrent steps of one template build once.

## Failure at each stage

Proof failures are validate errors (joined, field-pathed). Build and load
failures abort before any container exists — a step never starts against a
half-built image. Run-stage actuation failures (daemon down, kill on cancel)
surface as wrapped Go errors; an in-box failure is a `RunResult` with a
non-zero exit code and belongs to the failure module's record, not to infra.
