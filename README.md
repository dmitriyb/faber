# faber

A generic containerized-agent workflow engine. You declare a workflow in one
`orchestrator.yaml`; faber compiles it to a deterministic JSON IR, builds
immutable agent images from pinned Nix package sets, and executes the workflow
as a host-side DAG of single-purpose containers ("boxes") — with security
enforced from outside every container, pluggable budget metering, and
journal-based resume.

Faber is **mechanism, not policy**. It knows `docker build`/`docker run`, a
workflow DAG, and a handful of pluggable interfaces. It never learns your
issue tracker, your review gate, or your agent vendor: all opinionated
behavior arrives as user config — opaque scripts, typed params, data-source
commands, and companion services on a docker network faber treats as opaque.

## How a run works

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

- **The box.** Every agent step runs in a fresh container built from a pinned
  Nix toolset (no Dockerfile, no repo content baked in). Inside, an
  engine-owned sequencer (`faber-box`) drives a fixed phase order: deterministic
  setup (credentials, host-key policy, clone from the gateway, commit signing),
  your opaque `context`/`prelude` hooks, one headless agent invocation, then
  schema-validated result extraction. A step is a lambda: its only durable
  state is its typed output plus repo state behind your gateway.
- **The boundary.** The thing inside the container is untrusted. Every control
  is an externally enforced `docker run` binding: an internal network whose
  only route out is your allow-listing egress proxy, a single pinned git
  remote, an ephemeral ssh-agent holding exactly one role key, and credential
  *handles* — no secret is ever materialized inside a box (the explicit
  opt-out mounts a token as a read-only tmpfs file, shredded after the run).
- **The DAG.** `${steps.X.field}` references *are* the edges. Bounded loops
  are frontend sugar, unrolled at compile time into conditional chains;
  `generate` fans a named sub-workflow out over items emitted by your
  data-source command at run time, deriving inter-instance edges from each
  item's `deps`.
- **Failure is a record, not an absence.** Every step emits a structured
  result; an unfavorable-but-valid output (a review verdict of `changes`) is
  not a failure. Failures fail-stop their dependency chain, run your declared
  `on_failure` cleanup, optionally retry from a clean slate, and land in an
  append-only journal keyed `(step-id, input-hash)` that powers `faber resume`.
  A run accepts one process at a time (an advisory lock in the run dir), and
  every on-disk artifact carries a schema version: resume fails closed —
  never guesses — across a format, IR-schema, or image-derivation change,
  with `--fresh` as the escape.

## Quickstart

```sh
go build -o bin/faber ./cmd/faber
CGO_ENABLED=0 GOOS=linux go build -o bin/faber-box ./cmd/faber-box

bin/faber validate --config orchestrator.yaml            # schema, wiring, types, cycles, nix resolution
bin/faber validate --config orchestrator.yaml --emit-ir  # inspect the compiled IR
bin/faber build    --config orchestrator.yaml            # nix-build + docker-load every template image
bin/faber run task --config orchestrator.yaml \
    --param repo=sandbox --param item=I-1                # execute; prints the run report + journal path
bin/faber resume <run-id>                                # skip journaled hits, restart at the first gap
bin/faber resume <run-id> --interactive <step-id>        # reopen a failed step's box with a shell
```

Everything wrong with a config surfaces at `validate` with field paths —
missing params, unknown output fields, type mismatches, reference cycles,
unresolvable packages — never mid-run. See [`examples/`](examples/) for
working configurations and [`docs/deployment.md`](docs/deployment.md) for
what a production host needs.

## orchestrator.yaml in one glance

```yaml
network:   {name: agents-internal, proxy: http://egress:8888}  # egress lock
remote:    {url: ssh://git@gateway/srv/git, host_key_file: ./keys/gw.pub}
credentials:
  resolver: ./hooks/get-token             # opaque: get_token(service) -> stdout
  services: {agent-api: {mode: proxy, endpoint: http://token-proxy:8402}}
identities: {implementer: {key: ./keys/implementer}}   # one key per box

templates:
  implement:
    build: {packages: [git, openssh, go, claude-code]}  # pinned nixpkgs set -> immutable image
    run:
      identity: implementer
      resources: {memory: 8g, cpus: 4}
      env: {FABER_AGENT_CLI: claude}    # which agent binary the box invokes (required; no vendor default)
    skill: implement                                    # agent skill, invoked headlessly
    hooks: {context: ./hooks/gather-context, on_failure: ./hooks/release}
    inputs:  {repo: {type: string, required: true}, item: {type: string, required: true}}
    output:  {branch: {type: string, required: true}, pr: {type: int, required: true}}

workflows:
  task:
    params: {repo: {type: string, required: true}, item: {type: string, required: true}}
    steps:
      - id: implement
        use: implement
        with: {repo: "${params.repo}", item: "${params.item}"}
      - id: review-cycle
        loop: {max: 3, until: steps.review.verdict == "approved", steps: [...]}
      - id: merge
        use: merge
        when: steps.review.verdict == "approved"
        with: {repo: "${params.repo}", pr: "${steps.implement.pr}"}
```

Step inputs bind from a closed set of sources: a workflow param, a generate
item field, a literal, or another step's output field. Conditions (`when`,
loop `until`) are CEL expressions over completed step results and params,
compiled at validate time.

## CLI

| Command | Purpose |
|---|---|
| `faber validate` | Load, desugar, wiring-check every workflow; prove package resolution; `--emit-ir` prints the canonical IR |
| `faber build` | Build template images via Nix `dockerTools.buildLayeredImage`; `--template` narrows |
| `faber run <workflow>` | Execute with `--param k=v`, `--budget unit=n`, `--max-parallel n`, `--metering path`, `--report-json path\|-` |
| `faber resume <run-id>` | Re-enter a journaled run; `--fresh` ignores the journal, `--interactive <step>` opens the failed box |
| `faber upgrade-check` | Read-only pre-upgrade guard: refuses while live or unfinished runs exist (`--force` acknowledges) |

Common flags: `--config` (default `orchestrator.yaml`), `--log-level`,
`--log-format` (JSON when not a TTY). Exit codes: 0 ok, 1 validation/run
failure, 2 usage.

Environment: `FABER_STATE_DIR` (journals + image manifest, default `.faber`),
`FABER_BOX_BIN` (sequencer binary, default next to `faber`),
`FABER_GIT_NAME`/`FABER_GIT_EMAIL` (box committer identity).

## Modules

| Package | Owns |
|---|---|
| `config` | YAML schema, typed params, desugaring to the IR, wiring validation, CLI, logging |
| `infra` | Typed docker/git/nix adapters (structured I/O, no stdout scraping), Nix image build, container-run primitive |
| `security` | Network/remote/identity binding contributions to the run argv, credential broker, isolation runtime knob |
| `agent` | The box: `faber-box` phase sequencer, hook contracts, agent invocation, result extraction |
| `metering` | `estimate/actual` budget hooks, endpoint fidelity tiers, reactive 429-defer floor, pluggable cost unit |
| `failure` | Structured step results, on_failure/retry policy, the run journal, resume/fresh/interactive recovery |
| `pipeline` | The IR executor: parallel scheduling, CEL conditions, generate expansion, run reporting |

External dependencies are exactly `yaml.v3`, `cel-go`, and `x/term`;
everything else is the Go standard library plus `os/exec` behind typed,
fakeable interfaces.

## Development

```sh
go build ./... && go vet ./... && go test ./...   # the gate; needs no docker, nix, or agent CLI
go test -tags realinfra ./infra/ ./pipeline/      # integration suite; needs docker + nix
```

The repository is spec-first: `spec/` holds the typed requirement graph this
implementation traces to (see `CLAUDE.md`), and every test names the
requirement it verifies. Requirements titled `Deferred:` are captured backlog
— v0.1 ships their stated first-pass behavior only (no step timeouts, manual
image GC, per-run journals without cross-run locking, JSON-only inter-step
artifacts, a plain max-parallel cap, post-run reporting only).

## License

Apache 2.0 — see [LICENSE](LICENSE).
