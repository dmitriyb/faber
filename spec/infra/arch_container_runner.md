# ContainerRunner — the single-step run primitive

## What it is

The one place in faber where a `docker run` argv is ever constructed. Every
agent step becomes exactly one call to ContainerRunner with a fully resolved
run spec: the image tag, the template's resource limits, the engine-owned
mounts and environment, the ordered binding fragment produced by the security
module's BindingSet, and the in-container entry argv. ContainerRunner assembles
the argv in a fixed order, starts the container, captures output, waits, and
returns the exit code — one container per step attempt, `--rm`, never reused.

## Argv assembly

The assembly order is fixed and golden-testable:

```
docker run --rm --name <deterministic-step-name>
  [--memory=<m>] [--cpus=<n>]                    # from run.resources, omitted if unset
  -v <result-host-dir>:<result-path>             # engine bind, rw (survives the container)
  -v <hook-scripts-dir>:<hooks-path>:ro          # engine bind, read-only
  -v <box-entry-binary>:<entry-path>:ro          # engine bind, read-only
  -v <workspace-path>                            # engine anonymous volume, disk-backed
  --tmpfs <bundle-path> --tmpfs /tmp --tmpfs <home>   # engine tmpfs, RAM
  -e KEY=VALUE ...                               # engine env, sorted keys, no secrets
  <binding fragment, spliced verbatim>           # network/remote/identity/credentials/runtime
  <image-tag> <entry argv...>
```

The container starts as **root** — the image carries no baked user. There is no
`--user` flag: `faber-box` chowns the writable mounts (workspace, tmpfs bundle,
tmp, home) to the run uid:gid carried in engine env (`FABER_RUN_UID`/
`FABER_RUN_GID`) and drops privileges before any hook or agent runs. This is why
the workspace can be a fresh (root-owned) volume and still be writable by the
non-root box.

The binding fragment is **opaque and verbatim**: ContainerRunner neither
parses, reorders, nor deduplicates it. The security module owns what a network
or identity binding means; ContainerRunner owns only where bindings sit in the
argv. This split is the pinned cross-module contract — security composes,
infra splices.

## Argv discipline

Mirroring the proven harness's run shape, the assembled argv never contains:

- a docker socket mount (the box cannot reach the daemon);
- any mount beyond the declared ones — the result-dir bind, the read-only hook
  scripts, the read-only box entry binary (the agent module's sequencer,
  mounted and set as the entry argv), the disk-backed `/workspace` volume, the
  tmpfs writables (bundle, tmp, home), and whatever the binding fragment
  declares (agent socket, secret file, cache volume);
- `--privileged`, `--user`, host networking, or host PID/IPC namespaces (the
  box runs as root only until its entry program drops to the run user);
- a secret in `-e` form — engine env carries step-contract values only
  (secrets travel as binding-declared handles or file mounts).

Resource limits (`--memory`, `--cpus`) are emitted whenever the template
declares them; there is no engine default that silently unbounds a box.

## Lifecycle

Start, stream, wait, return. Combined stdout+stderr is captured into a bounded
buffer (returned to the caller for the failure record) and simultaneously
streamed to the step's debug logger — the box's own machine-readable output is
the mounted `result.json`, never its stdout. On context cancellation
ContainerRunner kills the container by its deterministic name, waits a bounded
grace period, and returns the context error; because every container runs
`--rm` with a known name, a cancelled run leaves nothing behind. Pre-run setup
and post-run teardown hooks (agent spawn/kill, secret shredding) belong to the
BindingSet and run outside this component — ContainerRunner's contract is that
it always returns, success or kill, so teardown always gets its turn.

The return value is mechanical: exit code, captured output, timing. Deciding
what an exit code *means* — reading `result.json`, fabricating the fallback
record, classifying failure — belongs to the agent and failure modules.

Requirement implemented: Container run primitive.
