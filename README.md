# faber

*Mechanism, not policy: one YAML in, a DAG of sealed boxes out.*

[![release](https://img.shields.io/github/v/release/dmitriyb/faber)](https://github.com/dmitriyb/faber/releases)
[![go](https://img.shields.io/github/go-mod/go-version/dmitriyb/faber)](go.mod)
[![license](https://img.shields.io/github/license/dmitriyb/faber)](LICENSE)
[![ci](https://github.com/dmitriyb/faber/actions/workflows/ci.yml/badge.svg)](https://github.com/dmitriyb/faber/actions/workflows/ci.yml)

<!-- terminal recording: faber validate --emit-ir, build and run on examples/quickstart. Placeholder until recorded. -->

`faber` turns one `orchestrator.yaml` into a deterministic JSON IR, builds
immutable agent images from a pinned Nix package set, and executes the workflow
as a host-side DAG of single-purpose containers, the boxes. Every control that
matters is bound from outside the container; the box itself is untrusted.

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

## What it does

- **One YAML, one IR.** `faber validate` checks wiring, types, reference cycles and package resolution, with field paths; the same YAML always yields byte-identical JSON, so the plan is diffable and reviewable before anything runs.
- **Immutable images.** Every template's image is a function of its package list, overlay and nixpkgs pin — no Dockerfile, no repo content baked in.
- **One box per step, fixed phase order.** context → prelude → agent → postlude → result, driven by an engine-owned sequencer; the typed output is enforced at the container boundary. Egress proxy, one pinned git remote, one role key in an ephemeral ssh-agent and credentials as handles are all bound from outside.
- **A host-side DAG.** Topological and parallel, CEL conditions on edges, bounded loops unrolled at compile time, `generate` fan-out over a data-source command at run time, and an append-only journal that `faber resume` picks up from.
- **No domain words.** Trackers, review gates, spec tools and agent vendors are user configuration: opaque scripts, typed params, data-source commands and companion services on a docker network faber treats as opaque. A change that teaches faber one of them is wrong by construction.

Everything wrong with a config surfaces at `validate`, never mid-run. See [`docs/architecture.md`](docs/architecture.md).

## Install

Download the install script, verify it, then run it. Never `curl | sh`: a piped script cannot verify itself before it runs. Two binaries are installed side by side, `faber`, the host CLI, and `faber-box`, the in-container phase sequencer. Details, other shells and the trust model are in [`docs/install.md`](docs/install.md).

```bash
curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh     -o install.sh \
&& curl -fsSL https://github.com/dmitriyb/faber/releases/latest/download/install.sh.sig -o install.sh.sig \
&& ssh-keygen -Y verify -f <(printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n') \
     -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh \
&& sh install.sh \
&& rm -f install.sh install.sh.sig
```

Public key, pin it once:

```
dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing
```

<details>
<summary>fish, plain sh, verifying the archives directly, upgrading</summary>

- **fish** and **plain `sh`** variants of the block above: [`docs/install.md`](docs/install.md#primary-verified-install-script).
- **Maximal**: skip the script and verify the release archives themselves by SSHSIG, SLSA attestation or the Go checksum database: [`docs/install.md`](docs/install.md#maximal-verify-the-binary-archives-directly).
- **Upgrading**: `faber upgrade` replaces `faber` and `faber-box` as a pair through the same signed installer, forward-only, refusing while a run is live; `--check`, `--version`, `--rollback`, `--force`: [`docs/install.md`](docs/install.md#upgrading).

</details>

## Quick start

```sh
cd examples/quickstart                                # config paths are read relative to the working directory
faber validate --config orchestrator.yaml --emit-ir   # the IR: one node, typed inputs and output; needs nix, not docker
faber build    --config orchestrator.yaml             # nix-build the template image, load it into docker
faber run brief --config orchestrator.yaml --param topic="what our retry policy does"   # one box; prints the run report and the journal path
faber resume <run-id>                                 # continue a stopped or failed run from its journal
```

That is one full cycle: compile the config, build the image, run one claude box against a typed output schema, then resume from the journal. `run` needs a token; the four steps to wire it are in [`examples/quickstart/README.md`](examples/quickstart/README.md), and the sealed shape with an egress lock, a pinned git gateway and one role key per box is in [`docs/deploy.md`](docs/deploy.md). Every flag is in [`docs/commands.md`](docs/commands.md).

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

The difference in one line: the others orchestrate agents inside a process or a runner you already trust; faber treats every step as an untrusted box and keeps the workflow, the gates and the recovery outside it. LangGraph v1, CrewAI, Microsoft Agent Framework 1.0 and the OpenAI Agents SDK describe themselves as production releases; Dagger container-use is experimental and GitHub Agentic Workflows is in public preview.

## Learn more

- [`docs/architecture.md`](docs/architecture.md) — how a run works: the box, the security boundary, the DAG, the failure and resume model.
- [`docs/configuration.md`](docs/configuration.md) — the `orchestrator.yaml` schema by example.
- [`docs/commands.md`](docs/commands.md) — every subcommand and flag.
- [`docs/deploy.md`](docs/deploy.md) — host requirements, companion topology, credentials, operations.
- [`docs/install.md`](docs/install.md) — install channels, the trust model, the public key, upgrading.
- `spec/**` — the authoritative, requirement-level specification (spexmachina format).

## License

Apache-2.0, see [`LICENSE`](LICENSE).
