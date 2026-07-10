# Data flow: YAML to IR pipeline

The frontend pipeline every faber invocation runs before anything touches docker.

```
orchestrator.yaml (bytes)
        │  os.ReadFile + yaml.v3
        ▼
*Config (typed, unvalidated)                    Loader.Load
        │  schema-level checks: unions, names,
        │  name-level refs, binding syntax
        ▼
*Config (schema-valid)                          Loader.Validate
        │  reuse resolution, loop unrolling,
        │  binding expansion, canonical emission
        ▼
IR (canonical JSON, per workflow)               Desugarer.Desugar
        │  resolution, slots, types, cycles,
        │  conditions, tool subset
        ▼
IR (proven wiring)                              WiringChecker.CheckWiring
        │
        ├──► stdout (--emit-ir, golden files)
        └──► pipeline.Execute (run) / infra.ProvePackages (validate)
```

## Shapes at each boundary

| Boundary | Shape | Contract |
|----------|-------|----------|
| bytes -> Config | yaml.v3 unmarshal | wrapped error with path + line on failure |
| Config -> Config | same value | multi-error or nil; no mutation |
| Config -> IR | `IR{ir_version, workflow, nodes[], edges[]}` | pure function, deterministic bytes, no I/O |
| IR -> IR | same value | multi-error with IR-node paths or nil; no mutation |
| IR -> executor | the same canonical struct | embeds resolved templates; executor never reads Config |

## Error paths

Each stage refuses to run on a prior stage's failure — but within a stage, all
violations are collected. `faber validate` therefore reports in at most three
waves (load errors; schema errors; desugar+wiring+package errors together),
each wave complete. There are exactly two hard stops: unreadable/unparseable
YAML, and a workflow-reference cycle (nothing to unroll).

## Who runs it

- `faber validate`: the whole flow for every workflow, plus infra's package
  proof; `--emit-ir` taps the final artifact.
- `faber run` / `resume`: the same flow in-process for the entry workflow (and
  transitively every workflow reachable via `use:`/`generate:`), then execution.
  The IR hash recorded in the journal is the hash of these bytes — resume
  compatibility is defined as "same bytes out of this pipeline".
