# faber

**A workflow engine for containerized agents: one YAML in, a host-side DAG of sealed single-purpose boxes out.**

[![Release](https://img.shields.io/github/v/release/dmitriyb/faber)](https://github.com/dmitriyb/faber/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/dmitriyb/faber)](go.mod)
[![License](https://img.shields.io/github/license/dmitriyb/faber)](LICENSE)
[![CI](https://github.com/dmitriyb/faber/actions/workflows/ci.yml/badge.svg)](https://github.com/dmitriyb/faber/actions/workflows/ci.yml)

<!-- recording placeholder: faber validate --emit-ir on examples/quickstart, then a quickstart run -->

```
orchestrator.yaml ──validate──▶ JSON IR (acyclic, byte-deterministic)
                                   │
                        host-side scheduler (topological, parallel, CEL conditions)
                                   │            ┌─ journal (resume) ─┐
                            one box per step ───┤  metering (admit)  │
                                   │            └─ on_failure hooks ─┘
              ┌────────────────────┴────────────────────┐
              │ container: context → prelude → agent →  │
              │ result   (fixed phase order, engine-owned) │
              └─────────────────────────────────────────┘
```

Faber is **mechanism, not policy**: it knows `docker build`/`docker run`, a workflow DAG, and a handful of pluggable interfaces.
It never learns your issue tracker, your review gate, or your agent vendor; all opinionated behavior arrives as user config: opaque scripts, typed params, data-source commands, and companion services on a docker network faber treats as opaque.
See [`docs/architecture.md`](docs/architecture.md) for the full model.

## What it does

- **Compiles one `orchestrator.yaml` to a deterministic IR.** Wiring, types, reference cycles and package resolution are checked at `faber validate`, with field paths; the same YAML always yields byte-identical JSON, so the plan is diffable and reviewable before anything runs.
- **Builds immutable images from a pinned Nix package set.** No Dockerfile and no repo content baked in; an image is a function of its package list, overlay and nixpkgs pin.
- **Runs every step in its own box with a fixed phase order.** context, prelude, agent, postlude, result, driven by an engine-owned sequencer. The typed output is enforced at the container boundary, and every security control (egress proxy, one pinned git remote, one role key in an ephemeral ssh-agent, credentials as handles) is bound from outside the untrusted container.
- **Schedules the DAG on the host.** Topological and parallel, with CEL conditions on edges, bounded loops unrolled at compile time, `generate` fan-out over a data-source command at run time, and an append-only journal that `faber resume` picks up from.
- **Knows no domain words.** Trackers, review gates, spec tools and agent vendors are all user configuration; a change that teaches faber one of them is wrong by construction.

## Install

Two binaries are published on the [GitHub Releases page](https://github.com/dmitriyb/faber/releases): `faber` (the host CLI, linux/darwin, amd64/arm64) and `faber-box` (the in-container phase sequencer, linux only).
The install script is verified against the release signing key before it runs, and the script verifies both binaries the same way and installs them side by side.

```bash
curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh     -o install.sh \
&& curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh.sig -o install.sh.sig \
&& ssh-keygen -Y verify -f <(printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n') \
     -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh \
&& sh install.sh \
&& rm -f install.sh install.sh.sig
```

<details>
<summary>fish</summary>

```fish
curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh -o install.sh
and curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh.sig -o install.sh.sig
and ssh-keygen -Y verify -f (printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' | psub) -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
and sh install.sh
and rm -f install.sh install.sh.sig
```

</details>

<details>
<summary>plain sh (no process substitution)</summary>

```sh
printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
sh install.sh
```

</details>

<details>
<summary>maximal: verify the archives directly, no script</summary>

```bash
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file \
  -s faber_<version>_<os>_<arch>.tar.gz.sig < faber_<version>_<os>_<arch>.tar.gz
gh attestation verify faber_<version>_<os>_<arch>.tar.gz --repo dmitriyb/faber
go install github.com/dmitriyb/faber/cmd/faber@<tag>
CGO_ENABLED=0 GOOS=linux go install github.com/dmitriyb/faber/cmd/faber-box@<tag>
```

The same checks apply to `faber-box_<version>_linux_<arch>.tar.gz`.

</details>

<details>
<summary>upgrading</summary>

`faber upgrade` moves `faber` and its coupled `faber-box` to the latest signed release in place, as a pair, through the same signed installer embedded in the binary. It refuses while a run is live or unfinished (`--force` overrides), is forward-only and refuses a "latest" older than what is installed; `--version vX.Y.Z` installs an exact release, `--rollback` restores the previous pair, `--check` resolves and verifies without changing anything.

</details>

The public key, what each channel protects, and the residual risks are in [`docs/install.md`](docs/install.md).

## Quick start

`faber validate` needs nix (it proves every package against the pinned nixpkgs snapshot, fetched over the network the first time). `faber build` and `faber run` need docker. Config paths are read relative to the working directory, so start in the example's directory.

```sh
cd examples/quickstart
faber validate --config orchestrator.yaml --emit-ir
```

This prints the canonical IR: one `agent` node, its template with the pinned package set, and the typed inputs and output schema. Nothing is built.

```sh
faber build --config orchestrator.yaml
faber run brief --config orchestrator.yaml --param topic="what our retry policy does"
```

`build` nix-builds the template image and loads it into docker.
`run` resolves the agent credential through `hooks/get-token` (a stub that exits 1 until you point it at your secret store), starts one box, enforces the declared `{summary, confidence}` output at the container boundary, and prints the run report and the journal path.
A stopped or failed run continues from its journal, skipping every step that already settled:

```sh
faber resume <run-id>
```

Everything wrong with a config surfaces at `validate` with a field path, never mid-run.


## Examples

[`examples/quickstart`](examples/quickstart/) is the one shipped example: a single template, a single workflow, one claude box with a typed output, and the credential stub above.
Its README lists every step from token to run.
The sealed shape, with an egress lock, a pinned git gateway and one role key per box, is described in [`docs/deploy.md`](docs/deploy.md) and specified under `spec/security/`.

## How it compares

Checked against each project's documentation on 2026-09-08. Two cells the docs do not state are marked as such.

| | Workflow written as | A step runs in | Next step decided by | Crash recovery |
|---|---|---|---|---|
| LangGraph | Python code, nodes are functions | the LangGraph runtime, inside your application | conditional edges in code | checkpointer, per thread |
| CrewAI Flows | Python decorators (`@start`, `@listen`, `@router`) | not documented | `@router` and `@listen` in code | `@persist` state, reloaded on restart |
| Microsoft Agent Framework | C#, Python or Go code, executors and edges | in-process execution | edge conditions in code | superstep checkpoints, resume |
| OpenAI Agents SDK | Python code | the runner loop in your application | the model, through handoffs exposed as tools | none built in; optional Temporal integration |
| Temporal | code in Go, Java, TypeScript or Python | worker processes you operate | workflow code | event-history replay (durable execution) |
| Dagger container-use | no workflow file; an MCP server plus agent prompts | a fresh container per agent, on its own git branch | the coding agent | container state tracked; the agent resumes |
| GitHub Agentic Workflows | Markdown plus YAML front matter, compiled to a lock file | GitHub Actions runners | declarative `on:` triggers; safe-outputs gate writes | no automatic retries, one run per trigger |
| faber | one YAML, compiled to a deterministic JSON IR | a fresh container per step, bound from outside | CEL conditions compiled at validate, never the model | journal keyed by step and input hash, `faber resume` |

The difference in one line: the others orchestrate agents inside a process or a runner you already trust, while faber treats every step as an untrusted box and puts the workflow, the gates and the recovery outside it.
LangGraph v1, CrewAI, Microsoft Agent Framework 1.0 and the OpenAI Agents SDK describe themselves as production releases; Dagger container-use is experimental and GitHub Agentic Workflows is in public preview.

## Learn more

- [`docs/architecture.md`](docs/architecture.md): how a run works, the box, the security boundary, the DAG, the failure and resume model.
- [`docs/configuration.md`](docs/configuration.md): the `orchestrator.yaml` schema by example.
- [`docs/commands.md`](docs/commands.md): the full CLI reference.
- [`docs/deploy.md`](docs/deploy.md): host requirements, companion topology, credentials, operations.
- [`docs/install.md`](docs/install.md): every install channel, the public key, upgrading.
- `spec/**`: the authoritative, requirement-level specification (spexmachina format).

## License

Apache-2.0 (see `LICENSE`).
