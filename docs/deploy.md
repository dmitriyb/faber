# Deploying faber

Faber is a single host-side binary plus an in-container sequencer. There is no
daemon, no database, and no control plane: state is a directory of append-only
run journals, and everything security-critical is either a `docker run`
binding or a companion service you already operate. Deploying it is therefore
mostly about preparing the host and the companion topology.

Both binaries — `faber` (the host CLI) and `faber-box` (bind-mounted
read-only into every box; a static `linux` binary regardless of the host's
own OS) — come from the signed GitHub release; see the README's install
section. `faber` looks for `faber-box` next to its own executable by
default; set `FABER_BOX_BIN` to relocate it. Building from source instead
(`go build ./cmd/faber`, `CGO_ENABLED=0 GOOS=linux go build ./cmd/faber-box`)
works identically — the release pipeline runs the same build, just
cross-compiled and signed; see `spec/delivery/arch_release.md`.

## Host requirements

| Component | Why | Notes |
|---|---|---|
| Linux host (or VM) | boxes, tmpfs secret mounts, ssh-agent sockets | the engine targets Linux; macOS works via a docker VM (see socket-group note below) |
| Docker daemon | runs the boxes | rootful or rootless; the CLI is driven with structured output modes only |
| Nix ≥ 2.4 | builds template images | `nix-command` is passed via `--extra-experimental-features`; images come from `dockerTools.buildLayeredImage`, no Dockerfiles |
| ssh-agent, ssh-add, ssh-keygen | ephemeral identity agents | stock OpenSSH |

Verify a build against a real daemon and nix:

```sh
go test -tags realinfra ./infra/ ./pipeline/   # build/run round trips, package proof, kill-on-cancel
faber validate --config examples/quickstart/orchestrator.yaml
faber build    --config examples/quickstart/orchestrator.yaml
```

## State directory

`FABER_STATE_DIR` (default `.faber`, resolved against the working directory)
holds:

- `runs/<run-id>/journal.jsonl` — the append-only run journal plus per-box attempt directories (result records, failure handoffs). This is the only state `faber resume` needs; treat it as disposable-after-triage or archive it for audit. Journals never contain resolved input values or secrets — only hashes, statuses, and error records.
- `infra/images.jsonl` — append-only bookkeeping of loaded images for a
  future GC command.

One journal directory per run; nothing coordinates across runs in v0.1, so
give concurrent pipelines distinct state dirs (or accept append-only
coexistence under `runs/`). Run it per-project: the state dir beside the
`orchestrator.yaml` that produced it keeps resume's config-path re-derivation
working.

Image and Nix-store growth is **manual GC** in v0.1: prune with
`docker image prune` / `nix-collect-garbage` on your own schedule; superseded
`faber/<template>:<hash>` tags are safe to delete (a rebuild recreates them
deterministically).

## Companion topology

Faber deliberately owns no network policy. You run three small services on an
internal docker network; faber only attaches boxes to that network and passes
opaque values through:

```yaml
# compose.yaml — sketch; harden to taste
networks:
  agents-internal:
    internal: true          # no route out except through the proxy
  egress:                   # proxy's outbound leg

services:
  gateway:                  # the only git remote a box can reach
    image: your/git-gateway # holds the forge credential; enforces signed
    networks: [agents-internal, egress]   # commits, fingerprint->role rules,
                                          # opens/merges PRs upstream
  egress:                   # allow-listing forward proxy
    image: your/proxy       # allow-list: your agent API endpoints, nothing else
    networks: [agents-internal, egress]

  token-proxy:              # optional: auth-injecting proxy for `mode: proxy`
    image: your/token-proxy # credentials
    networks: [agents-internal, egress]
```

Then in `orchestrator.yaml`:

```yaml
network:  {name: agents-internal, proxy: http://egress:8888, no_proxy: [gateway]}
remote:   {url: ssh://git@gateway/srv/git, host_key_file: ./keys/gateway_host_key.pub}
```

The trust chain: the box can only reach the internal network; its only remote
is the gateway, pinned by host key; the gateway holds the real forge
credential and enforces role rules server-side (signed commits, branch and
content policy). Faber guarantees one identity key per box; your gateway maps
fingerprints to permissions. Nothing faber-side needs to be trusted with the
upstream credential.

## Keys and credentials

- **Identities**: `identities: {<role>: {key: <ref>}}` — the ref is interpreted by your resolver/tooling (file path, hardware-key handle). Per step, faber spawns an ephemeral ssh-agent, loads exactly that one key, and forwards only the socket; the private key never enters the box. On macOS docker VMs, set the identity binding's socket group if the box user can't read the forwarded socket.
- **Host key pinning**: commit your gateway's public host key and reference it as `remote.host_key_file`. `tofu: true` (accept-new) is for sandboxes only; configuring neither aborts the run — there is no silent fallback.
- **API credentials**: prefer `mode: proxy` (auth injected outside the box) or `mode: helper`; `mode: file` is the explicit degraded path — the resolver's token is written to a tmpfs-backed read-only mount and shredded after the step, but it *is* readable inside that box. The resolver (`credentials.resolver`) is any executable: `get_token(service)` on argv, token on stdout, invoked host-side only. Wire it to `pass`, `rbw`, your keychain, or your secrets manager.
- **Commit identity**: set `FABER_GIT_NAME` / `FABER_GIT_EMAIL` for the committer identity boxes use; signing keys always come from the forwarded agent.

## Budgets and metering

Measurement is run policy, not workflow config: pass `--budget unit=N` and a
`--metering <file>` describing how each endpoint class is measured:

```yaml
endpoints:
  local-llm:
    tier: exact            # tokenizer-computed hard upper bound at admission
    unit: tokens
    tokenizer: {command: ./meters/tokenize, max_output: 4096}
    templates: [implement, fix]
  vendor-api:
    tier: reported         # trust the response usage sidecar, retrospectively
    unit: usd-cents
    fields: {billed_cents: usd-cents}
  subscription:
    tier: probe            # best-effort saturation probe — your policy, never a default
    probe: {command: ./meters/probe, threshold: 0.9}
```

With no metering config at all you still get the reactive floor: a step
failing with a rate-limit signal and reset time is deferred until the reset,
not failed. Budgets are unit-tagged; only meters reporting a budget's unit
gate admission against it, and a budget nothing measures logs a warning at
startup rather than silently not enforcing.

## Hardening knobs

- `run.runtime: runsc` (per template) switches the container runtime (e.g.
  gVisor) for hardened isolation — argv-only, the image is unchanged.
- `network.nftables: true` is the proxy-less egress mode: baked rules loaded by a root entrypoint with `NET_ADMIN`, dropping to the agent user. It is an explicit choice; a network section with neither `proxy` nor `nftables` fails validate, so a capability grant can never happen by omission.
- Template env and volumes are screened: engine- and security-owned names (`FABER_*`, `SSH_AUTH_SOCK`, reserved mount paths) are rejected at validate or spec-build time rather than silently overridden.

## Operations

- **Failures**: the run report names each failed step, its reason, and a ready-made `faber resume <run-id> --interactive <step>` line that reopens the failed box (same image, bindings, inputs, with the failure handoff mounted read-only) for diagnosis.
- **Resume**: `faber resume <run-id>` skips journal hits by `(step-id, input-hash)` — a changed input, template, or image tag re-runs automatically. The journal pins the IR hash; if the config drifted, resume refuses (fix the config back, or `--fresh` to re-run everything under a new run id).
- **Upgrades**: `faber upgrade` updates faber and its coupled `faber-box` to the latest signed release in place — it runs the `upgrade-check` guard first (below), then replaces both binaries as a unit via the embedded, already-verified `install.sh` (`--check`/`--dry-run`, `--version vX.Y.Z`, `--rollback`, `--force`). `faber upgrade-check` is the same guard on its own — the read-only pre-flight encoding "faber is not upgraded mid-run": non-zero while any run is live (its lock is held) or unfinished (no run-end marker in its journal), listing them; `--force` acknowledges. The binary swap is otherwise external. On-disk artifacts are schema-versioned (journal format, IR version, the faber↔faber-box contract, per-template image tags recorded at run start), and resume fails closed on any mismatch — an engine-side change is named as such, never blamed on your config, and there is no auto-migration: finish in-flight runs on the old binary or `--fresh` them. Same YAML must still produce byte-identical IR (`validate --emit-ir` is diffable and golden-tested).
- **Abort**: SIGINT/SIGTERM cancels the run; in-flight containers are killed by name and bindings torn down. There are no per-step timeouts in v0.1 — bound runaway steps with metering budgets or external supervision.

## CI

The repo's own gate (`go build ./... && go vet ./... && go test ./...`, `-race`
on `main`, nightly fuzz) is hermetic — no docker, nix, or agent CLI needed;
see `spec/delivery/arch_ci.md`. The `realinfra`-tagged suite
(`go test -tags realinfra ./...`) needs a real docker daemon and, for
`infra/`'s cases, a working `nix` with network access to the pinned
nixpkgs — it stays a local/acceptance-machine suite, not a hosted-CI one
(see `spec/delivery/arch_ci.md` for why). Run it on a docker+nix machine
before trusting a build against your real configs and staging companion
services.
