# Project Proposal: Faber

*A generic containerized-agent workflow engine.*

## Vision

The autonomous-agent harness that this engine generalizes exists today as a dogfooded tree of shell scripts inside a personal dotfiles repo: a host-side runner and epic orchestrator, in-container start scripts with a deterministic prelude, an egress-locked docker network, per-role forwarded signing keys, and a server-side push gate. It works, but it is coupled to the dotfiles repo (agent images clone and stow the whole repo), so it is not shareable, and imperative shell orchestration does not scale to richer workflows.

Faber extracts the engine into a standalone Go project. The guiding principle is **mechanism vs. policy**: faber is a generic engine that knows `docker build`/`docker run`, a workflow DAG, and pluggable credential/metering interfaces. It must **never learn domain words** — no issue tracker, no gate-service name, no spec tool. The opinionated secure harness (the push gate, the egress lock, role keys, the spec-driven prelude) remains **user config**: one `orchestrator.yaml`, a container recipe, and companion services on a named docker network that faber treats as opaque. This dissolves both problems at once — faber becomes shareable, the personal setup stays personal, and the shell tree collapses into a Go engine plus declarative data.

Faber evolves `github.com/dmitriyb/conductor` — a clean, generic Claude-agent DAG runner — but with a **new spec written fresh** from the settled architecture rather than a retrofit, because conductor omits the security pillars (egress lock, gateway pinning, role-key delegation) that are the differentiator, and its config model predates the typed IR.

## Architecture (settled design)

**Two levels only.** There is no third abstraction; an "epic" is just a workflow with more steps.

1. **Lifecycle template — "the box".** A container recipe with a *fixed* internal phase order `context → prelude → agent → result`. The user fills the phases; the sequence is baked (it is not a configurable in-container DAG). Split into **build** (Nix package set → immutable image) and **run** (`docker run` bindings).
2. **Workflow — the host-side DAG** that faber coordinates. The boundary rule between "in the box" and "faber coordinates" is the **role/identity boundary**: anything that crosses a signing identity (implement → review → fix → merge) is host-side; everything within one identity stays in-box.

**IR (the backend, ONNX-style).** A uniform, acyclic graph of node kinds: `agent` (leaf — run a box), `sub-workflow` (compose), `generate` (expand a named sub-workflow over a data-source's items, with edges derived from the items' declared dependencies). Bounded loops (`loop {until, max: N}`) are **frontend sugar unrolled** into a linear conditional chain — the executed IR stays a pure DAG with no Loop op. **YAML in (people), JSON IR out (machine)**: the frontend is a compact YAML schema that desugars to the JSON IR (Compose→runtime style), with `$ref`-style reuse of named sub-workflows. TOML and raw JSON were rejected for authoring ergonomics; a configuration *language* (Dhall/CUE) was rejected as overengineering.

**Typed data threading.** Every step is a typed function: its template declares `inputs` (named, typed slots) and an `output` schema. A step's `with:` binds slots from a closed set of sources — a workflow **param**, a `generate` **item** field, a **literal**, or another step's output field (`${steps.X.field}`). **A reference *is* the DAG edge** — data dependency and execution dependency are the same thing; explicit `depends_on` exists only for pure ordering. Step outputs are schema-validated *before* being threaded onward, so wiring, type, and cycle errors surface at `faber validate`, never mid-run.

**Step = lambda.** Each step is a separate container whose only durable state is its typed output plus the repo (reached via the user's gateway). This is *why* resume works: cleanup is scoped to one failed step's side-effects, prior outputs survive in the journal, and you never resume *into* an agent's chain-of-thought — a step is atomic; re-run the whole step.

**Tools = one list = the environment.** A template's `tools:` is the Nix package set — pinned, root-owned, immutable at runtime. The agent runs **unrestricted inside** (`bypassPermissions`); the environment *is* the restriction — there is no second in-container permission gate. Validation is one-directional: the image is built from the list (`nix eval` proves every name resolves), and a step's declared tool-needs must be a subset of its template's list.

## Security model

Principle: **every real control is enforced from outside the container or is immutable relative to it, because the thing inside is untrusted.** The image is a pure function of the toolset; security lives in the `docker run` argv plus external companion services.

- **Egress lock → a `network` binding.** `--network <internal-net>` plus `HTTPS_PROXY=<proxy>`: the `--internal` docker network has no route out; an external dual-homed allow-listing proxy is the only egress. A valid alternative binding: **baked nftables** loaded by a root entrypoint (`--cap-add NET_ADMIN`) that then drops to the non-root agent user — immutable to the agent and self-contained, at the cost of the capability.
- **The gate → a `remote` binding.** The box clones and pushes `ssh://git@<gateway>/…` with a pinned host key. The gate is an **external companion service** that validates every push (signed commits, key-fingerprint→role mapping, content rules) and holds the forge PAT. Faber only ever sees a git URL.
- **Role identity → an `identity` binding per template.** Faber spawns an **ephemeral single-key ssh-agent** for the template's role and forwards the socket into the container. One key per container ⇒ role isolation by construction. The prelude derives `user.signingkey` from the forwarded agent — the same key signs commits and authenticates SSH. Fingerprint→role *enforcement* belongs to the gate, not faber.
- **Credential delegation (uniform rule).** Never materialize a secret inside the container; forward a **handle to an out-of-container broker**: an ssh-agent socket for keys; a base-URL pointing at an auth-injecting proxy for API tokens (the container reaches an *unauthenticated* local endpoint; the proxy injects the credential). The handle shape is per-tool (socket / endpoint / credential-helper) because no universal "token-agent" exists. Raw-token-in-RAM (`/dev/shm`, `chmod 600`, `--rm`) is the explicit **degraded** opt-out. `get_token(service)` is a generic resolver the user supplies (keychain / rbw / env / static) — faber is agnostic to how.
- **Isolation runtime.** macOS already gets VM isolation (Docker Desktop). On Linux, an optional `runtime` knob maps to `--runtime=runsc` (gVisor). A proprietary microVM was rejected: it hardens a boundary the gate model deliberately treats as expendable.

## Failure, resumption, budget

- Every step emits a **structured result** (`{status: ok|failed, payload|error, …}`) — failure is a record, not an absence. An unfavorable-but-valid output (review says "changes") is **not** a failure.
- **Fail-stop** on failure, plus a declared **`on_failure` cleanup hook** (an opaque user script — delete the orphan branch, release the claimed item). `retry: N` is opt-in; cleanup runs between attempts, which is what guarantees retry idempotency.
- Loops are bounded (`max` reached ⇒ failure). **Resume** works from a run **journal** keyed `(step-id, input-hash)`: cached good steps are skipped; execution restarts at the first failed or absent step. Three recovery modes: **resume** (reuse journal), **fresh** (`--no-cache`), **interactive** (drop into the failed box with its tools). A "partial epic" is not special-cased — a failed fan-out item is a normal failure plus journal entry.
- **Budget/usage is a pluggable pre/post-step hook** (`estimate(step) → cost` before, `actual(result) → cost` after), with fidelity per endpoint class: local/OSS = tokenizer-exact hard-upper-bound admission; vendors with usage reporting = read the usage block; subscription endpoints without usage APIs = best-effort probes supplied as user policy, never a faber default. The floor under all of it: **reactive 429-defer** (defer to the reported reset epoch). The budget *unit* is pluggable (tokens / dollars / GPU-seconds).

## Modules

Seven modules. The first four evolve conductor's subsystems; the last three are new pillars this design introduces.

### 1. config

The foundation. Defines the `orchestrator.yaml` schema (workflows, lifecycle templates, typed params), loads and validates it, and **desugars** the YAML frontend into the acyclic JSON IR (unrolling bounded loops, resolving `$ref` sub-workflow reuse, turning `${steps.X.field}` references into typed edges). Owns the typed-params interface (name/type/required, validated before anything runs; `repo` is an optional param — never baked into an image), the CLI (`run`, `validate`, `build`, `resume`), and structured logging.

### 2. infra

Typed subprocess actuation. Wraps `docker`, `git`, and `nix` CLIs behind clean Go interfaces with structured I/O (JSON output modes, never stdout-scraping). Builds images with Nix `dockerTools.buildLayeredImage` from a template's pinned package list (custom bins arrive via a user-supplied overlay of derivations), proves package names resolve at validate time (`nix eval`), and provides the container-run primitive that assembles a `docker run` argv from bindings supplied by other modules.

### 3. security

The run-time bindings and the credential-delegation interface: `network` (internal net + proxy env, or the nftables alternative), `remote` (gateway URL + pinned host key material), `identity` (ephemeral single-key ssh-agent lifecycle + socket forwarding), the broker-handle model for API credentials, the `get_token` resolver seam, the degraded raw-token mode, and the `runtime` isolation knob.

### 4. agent

The box. Owns the fixed in-container phase order `context → prelude → agent → result`: deterministic context/prelude hooks (opaque user scripts), agent invocation (`bypassPermissions` inside the sealed environment), and structured-result extraction and schema validation at the container boundary.

### 5. metering

The pluggable budget hook: `estimate(step) → cost` admission before each step, `actual(result) → cost` accounting after, endpoint fidelity tiers, the reactive 429-defer floor, and the pluggable cost unit.

### 6. failure

The structured step-result contract, fail-stop semantics, `on_failure` cleanup hooks, opt-in retry with between-attempt cleanup, the run journal keyed `(step-id, input-hash)`, and the three recovery modes (resume / fresh / interactive).

### 7. pipeline

The IR executor: topological scheduling with parallelism over the JSON IR, CEL condition evaluation on the unrolled conditional chains, `generate` expansion at run time (invoke the user's data-source command, receive `{items: [{id, deps, …}]}`, instantiate the named sub-workflow per item with edges from the items' deps), failure propagation, and run aggregation/reporting.

## What carries over from conductor, and what changes

| Conductor | Faber | Verdict |
|---|---|---|
| Four subsystems config/infra/agent/pipeline, implemented in dependency order | Same four, plus security/metering/failure | **Carries over** (structure) |
| Repo scaffold: Go module, `spec/` driven development, `.claude/skills`, stdlib-first (`yaml.v3`, `cel-go`, `x/term`) | Identical scaffold; spec now in spexmachina format | **Carries over** |
| CLI shelling to `docker`/`git` via `os/exec`, no SDKs | Same, plus `nix`, and hardened to structured-I/O typed interfaces only | **Carries over, tightened** |
| DAG construction, Kahn topological sort, cycle detection, CEL conditions, parallel goroutine scheduling, failure propagation | Same algorithms, but executing the desugared JSON IR rather than the raw YAML step list | **Carries over, re-targeted** |
| Marker-based output (`###PIPELINE_OUTPUT###`) + lightweight schema check | Structured result contract at the container boundary, schema-validated before threading | **Evolves** |
| Free-form Go-template threading (`{{.Steps.x.Output.y}}`) | Typed slots: `inputs`/`output` schemas, `${steps.X.field}` references that *are* edges, validate-time wiring errors | **Replaced** |
| `agents:` map with prompt/workspace/tools per agent | Lifecycle templates: build (Nix packages) / run (bindings) split, fixed phase order, identity per template | **Replaced** |
| Dockerfile generation from a base image | Nix `dockerTools.buildLayeredImage` from the pinned toolset; no Dockerfile | **Replaced** |
| `credentials.backend: rbw\|env\|file`, token injected into clone URL, env-file in `/dev/shm` | Credential delegation via broker handles (sockets/endpoints); resolver seam for rbw/env/etc.; raw-token-in-RAM demoted to explicit degraded mode | **Replaced** (survives only as the degraded path + resolver backends) |
| `project.repository` baked in config | `repo` is a typed run-time param, optional, per-step overridable | **Replaced** |
| No egress control, no gateway, no role signing, no metering, no journal/resume | security, metering, failure modules | **New** |
| Single flat pipeline, no fan-out, no loops | `sub-workflow`, `generate`, unrolled bounded loops in the IR | **New** |

## Key requirements

### Functional

1. **Declarative workflow configuration** — one `orchestrator.yaml` declares lifecycle templates, workflows with typed params, and steps; loaded and validated as a whole before anything runs.
2. **Desugared acyclic IR** — the YAML frontend compiles to a JSON IR of `agent` / `sub-workflow` / `generate` nodes; bounded loops unroll to conditional chains; the executed graph is always a pure DAG.
3. **Typed data threading** — steps declare typed input slots and output schemas; `${steps.X.field}` references are the DAG edges; wiring/type/cycle errors are validate-time errors.
4. **Immutable toolset images** — images are built from a template's pinned Nix package list; a repo is never baked into an image; one image serves any repo.
5. **The box lifecycle** — every agent step runs the fixed phase order context → prelude → agent → result inside a fresh container; phases are user-filled, order is engine-owned.
6. **Host-side DAG execution** — topological, parallel, CEL-conditional execution of the IR, including run-time `generate` expansion over user data-source items.
7. **Externally enforced security bindings** — network (egress lock), remote (pinned gateway), and identity (ephemeral per-role ssh-agent) bindings assembled into the `docker run` argv from config.
8. **Credential delegation via handles** — secrets never materialize in a container; brokers are forwarded as handles; the resolver (`get_token`) is user-supplied; raw-token-in-RAM is an explicit degraded mode.
9. **Pluggable metering** — estimate/actual budget hooks with per-endpoint fidelity tiers, a reactive 429-defer floor, and a pluggable cost unit.
10. **Structured failure and resume** — structured step results, fail-stop, `on_failure` cleanup, opt-in retry with between-attempt cleanup, a journal keyed `(step-id, input-hash)`, and resume/fresh/interactive recovery.
11. **Reference workflows acceptance** — the reference `orchestrator.yaml` (task workflow: implement → review/fix loop; epic workflow: generate task over items + merge) validates and runs; it is the completeness test that the dogfooded harness translates into pure config.

### Non-functional

1. **Mechanism, not policy** — faber never learns a domain word; every opinionated behavior enters through config seams (opaque scripts, params, data-source commands, companion services).
2. **Untrusted box** — every control is enforced from outside the container or is immutable relative to it.
3. **Validate before run** — all schema, wiring, type, cycle, and package-resolution errors surface at `faber validate`.
4. **Stdlib-first Go** — external deps limited to `yaml.v3`, `cel-go`, `x/term`; subprocesses behind typed structured-I/O interfaces.
5. **Deterministic desugaring** — the same YAML always produces the same IR, byte for byte; the IR is inspectable (`faber validate --emit-ir`).

## Open edge cases (deferred backlog — captured in module requirements as `Deferred:` items)

1. Non-agent steps (human-approval / wait / pure-command IR node type) → config
2. `generate` edge cases: empty item set, malformed source output, dependency cascade when an item fails → pipeline
3. Inter-step artifacts beyond typed JSON (large files, research outputs, non-git deliverables) → agent
4. `context`/prelude genericity — gathering rich context without faber learning the user's spec tool → agent
5. Concurrency control — fan-out parallelism caps / scheduler awareness of host CPU, local GPU, API RPM → pipeline
6. Timeouts and cancellation — per-step wall-clock limits; user abort with in-flight container cleanup → failure
7. Secret/identity expiry mid-run (OAuth token, forwarded socket) and refresh policy → security
8. Observability — step logs/traces/live progress; inspecting a running workflow → pipeline
9. Cost/usage aggregation and reporting (roll per-step metering into a run total) → metering
10. Run journal/state-store location and concurrency (two runs claiming the same item: dedup/locking) → failure
11. Nix image GC / cache eviction → infra
12. Egress allow-list contents per endpoint class — who declares them, where they live → security
13. Gateless workflows — a research run has no push to gate; what is the trust boundary and where does terminal output live → security

## Design decisions

### YAML frontend, JSON IR backend

People author YAML; the engine executes JSON. The desugaring boundary (Compose→runtime style) keeps ergonomics and rigor from fighting: sugar (loops, `$ref` reuse, compact `with:` bindings) lives in the frontend; the backend IR is uniform, acyclic, and trivially checkable. TOML/raw JSON rejected for authoring; Dhall/CUE rejected as overengineering.

### References are edges

There is no separate dependency declaration for data. If step B consumes `${steps.A.branch}`, B depends on A — the type-checker and the scheduler read the same fact from the same place. `depends_on` survives only for pure ordering (rare).

### The role/identity boundary decides box vs. host

A workflow step boundary exists exactly where a signing identity changes. This single rule prevents both failure modes: a mega-box that implements-reviews-merges under one key (no role isolation), and a micro-DAG that fragments one identity's work into needless containers.

### Security is bindings, not image content

The image is a pure function of the toolset. Everything trust-relevant — network, gateway, keys, tokens, runtime — arrives as `docker run` bindings or lives in external companion services on a named docker network that the user brings up out-of-band (docker-compose) and faber treats as opaque.

### One structured result per step, journaled

The step result record is simultaneously the data-threading payload, the failure signal, the journal entry, and the metering input. One contract, four consumers.

### Go, stdlib-first, CLIs actuated behind typed interfaces

The value of the engine is the DAG/validation/threading logic, not the subprocess calls. Shelling out to `docker`/`git`/`nix`/user resolvers is fine — but only behind interfaces with structured I/O, so the core stays testable with fakes.

## Relationship to the dogfooded harness

The existing shell harness (host-side runner + epic orchestrator, in-container start scripts, egress compose stack, gate service) is the **behavioral reference** for this spec: the task loop, the epic fan-out, the prelude's deterministic steps, the signing flow, and the gate contract are proven there. Faber generalizes that behavior into config-driven form. The translation is the acceptance example: templates `implement` / `review` / `fix` / `merge`; a **task** workflow (`implement → loop{until verdict == approved, max N}{review; fix when not approved}`); an **epic** workflow (`generate: task over ${sources.members}; merge after`). The only domain-specific pieces reduce to opaque scripts (prelude = claim item, context = gather spec) and a data-source command emitting `{items: [{id, deps}]}` — faber never learns what an "item" is.
