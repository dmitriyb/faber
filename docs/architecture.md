# How it works

Faber turns one `orchestrator.yaml` into a deterministic JSON IR, then executes it as a host-side DAG of single-purpose containers ("boxes"), with every security-relevant control enforced from outside the container:

- **The box.** Every agent step runs in a fresh container built from a pinned Nix toolset (no Dockerfile, no repo content baked in). Inside, an engine-owned sequencer (`faber-box`) drives a fixed phase order — deterministic setup (credentials, host-key policy, clone from the gateway, commit signing), your opaque `context`/`prelude` hooks, one headless agent invocation, then schema-validated result extraction. A step is a lambda: its only durable state is its typed output plus repo state behind your gateway.
- **The boundary.** The thing inside the container is untrusted. Every control is an externally enforced `docker run` binding: an internal network whose only route out is your allow-listing egress proxy, a single pinned git remote, an ephemeral ssh-agent holding exactly one role key, and credential *handles* — no secret is ever materialized inside a box (the explicit opt-out mounts a token as a read-only tmpfs file, shredded after the run).
- **The DAG.** `${steps.X.field}` references *are* the edges, resolved and validated at compile time into the IR. Bounded loops are frontend sugar, unrolled into conditional chains; `generate` fans a named sub-workflow out over items emitted by your data-source command at run time, deriving inter-instance edges from each item's `deps`. The scheduler runs the DAG topologically and in parallel, gating each edge on a CEL condition (`when`, loop `until`) compiled at validate time.
- **Failure is a record, not an absence.** Every step emits a structured result; an unfavorable-but-valid output (a review verdict of `changes`) is not a failure. Failures fail-stop their dependency chain, run your declared `on_failure` cleanup, optionally retry from a clean slate, and land in an append-only journal keyed `(step-id, input-hash)` that powers `faber resume`. A run accepts one process at a time (an advisory lock in the run dir), and every on-disk artifact carries a schema version: resume fails closed — never guesses — across a format, IR-schema, or image-derivation change, with `--fresh` as the escape.

This page is a summary; the authoritative, requirement-level specification is under `spec/**`:

- `spec/agent/arch_phase_sequencer.md`, `arch_agent_invoker.md`, `arch_prelude_hooks.md`, `arch_result_extractor.md` — the box's fixed phase order in full.
- `spec/security/arch_network_binding.md`, `arch_remote_binding.md`, `arch_identity_binding.md`, `arch_credential_broker.md` — every externally enforced binding.
- `spec/config/arch_desugarer.md`, `arch_ir_model.md` — the IR: loop unrolling, `$ref` resolution, reference-to-edge compilation.
- `spec/pipeline/arch_scheduler.md`, `arch_condition_evaluator.md`, `arch_generate_expander.md` — parallel scheduling, CEL conditions, `generate` fan-out.
- `spec/failure/arch_failure_policy.md`, `arch_journal.md`, `arch_recovery_modes.md` — on_failure/retry, the journal, resume/fresh/interactive recovery.

## Guiding principle

Faber is generic mechanism: every domain name (an issue tracker, a review gate, an agent vendor, a spec tool) is config faber ships none of.
It knows `docker build`/`docker run`, a workflow DAG, and pluggable credential/metering interfaces — nothing more.
A PR that teaches faber a domain word is wrong by construction; the fix is always to find the config seam instead.
