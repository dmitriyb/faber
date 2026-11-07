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
docker run --rm [-i] --name <deterministic-step-name>
  [--memory=<m>] [--cpus=<n>]                    # from run.resources, omitted if unset
  -v <workspace-path>                            # engine anonymous volume, disk-backed
  --tmpfs <result-path> --tmpfs <bundle-path> --tmpfs /tmp --tmpfs <home>   # engine tmpfs, RAM
                                                 # (no host path: faber-box, the hooks and /faber/skills are image
                                                 #  content; the result dir is a tmpfs the box ships out on its stdout)
  -e KEY=VALUE ...                               # engine env, sorted keys, no secrets
  <binding fragment, spliced verbatim>           # network/remote/identity/credentials/runtime
  <image-tag> <entry argv...>
```

The `-i` flag appears only when the run carries a `StdinSecrets` payload
(file-mode credentials); it attaches the container's stdin so the resolved
tokens can stream in as one JSON object. The `--tmpfs /run/secrets` that makes
that dir writable RAM arrives inside the binding fragment, like every other
credential flag.

The container starts as **root** — the image carries no baked user. There is no
`--user` flag: `faber-box` chowns the writable mounts (workspace, the tmpfs
result, bundle, tmp, home) to the run uid:gid carried in engine env
(`FABER_RUN_UID`/`FABER_RUN_GID`) and drops privileges before any hook or
agent runs. This is why the workspace can be a fresh (root-owned) volume and
still be writable by the non-root box. No mount is a host path: on a daemon
with a remapped user namespace the box's uid is not the host's, and a shared
directory would be unwritable; everything the box must hand back travels on
its stdout instead.

The binding fragment is **opaque and verbatim**: ContainerRunner neither
parses, reorders, nor deduplicates it. The security module owns what a network
or identity binding means; ContainerRunner owns only where bindings sit in the
argv. This split is the pinned cross-module contract — security composes,
infra splices.

## Argv discipline

Mirroring the proven harness's run shape, the assembled argv never contains:

- a docker socket mount (the box cannot reach the daemon);
- any mount beyond the declared ones — the disk-backed `/workspace` volume,
  the tmpfs writables (result, bundle, tmp, home; the entry binary, the hook
  scripts and the skills tree are image content under `/faber`, baked by the
  image builder, never mounts), and whatever the binding fragment declares
  (agent socket, the `/run/secrets` tmpfs, cache volume); no engine mount is a
  host bind;
- `--privileged`, `--user`, host networking, or host PID/IPC namespaces (the
  box runs as root only until its entry program drops to the run user);
- a secret in `-e` form — engine env carries step-contract values only
  (secrets travel as binding-declared handles or, for file mode, the stdin
  payload written into the container's `/run/secrets` tmpfs).

Resource limits (`--memory`, `--cpus`) are emitted whenever the template
declares them; there is no engine default that silently unbounds a box.

## Lifecycle

Start, stream, wait, return. When the run carries a `StdinSecrets` payload,
ContainerRunner opens the container's stdin (`-i`), writes the JSON once, and
closes it — a clean EOF the box reads as end-of-payload; stdin is otherwise
unattached. The two output streams are kept apart. The container's **stderr**
(plus the docker client's own messages) is the human channel: captured into
a bounded buffer (returned to the caller for the failure record), streamed to
the step's debug logger, and — when the spec carries an `Output` writer —
tee'd in full into it (the run-prep seam points it at the attempt's durable
`output.log`). The container's **stdout** is the box's machine channel, the
frame stream the agent module defines, and goes untouched to the spec's
`Stdout` writer (the run-prep seam's demuxer); ContainerRunner never parses
it, and a nil `Stdout` discards it. The stdin payload is never logged: it is
a credential. On context cancellation
ContainerRunner kills the container by its deterministic name, waits a bounded
grace period, and returns the context error; because every container runs
`--rm` with a known name, a cancelled run leaves nothing behind. Pre-run setup
and post-run teardown hooks (agent spawn/kill; file mode leaves no host residue
to undo) belong to the BindingSet and run outside this component —
ContainerRunner's contract is that it always returns, success or kill, so
teardown always gets its turn.

The return value is mechanical: exit code, captured output, timing. Deciding
what an exit code *means* — reading `result.json`, fabricating the fallback
record, classifying failure — belongs to the agent and failure modules.

## Entering a reconstructed box

Interactive recovery needs the operator's terminal inside a box that first
has to receive files (the failed attempt's handoff, a saved transcript) over
the frame channel, and a single `docker run -it` cannot take a stream first
and a terminal after. `Enter` is therefore a choreography of four verbs, all
of them infra's: start the reconstructed box detached with the entry
`faber-box hold` (`docker run -d --rm …`, the same argv assembly with `-d`
after `--rm`); feed the caller's frame stream to `docker exec -i <name>
faber-box receive`; attach the operator's terminal with `docker exec -it -u
<uid>:<gid> [-e …] -w <dir> <name> <entry>`; kill the container by name when
the terminal exits (`--rm` removes it). Nothing is mounted from the host, and
the container is addressable throughout by its deterministic name. `Enter`
always kills what it started, success or failure of any verb.

Requirement implemented: Container run primitive.
