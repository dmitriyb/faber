# CI (build/vet/test, race, fuzz)

Three triggers, tiered by cost, each in its own workflow file so a trigger change in one never risks the others.

## Fast gate (`pull_request`, `push` to `main`)

`.github/workflows/ci.yml` runs one job on both triggers:

```
go build ./...
go vet   ./...
go test  ./...
```

On `push` to `main` only, a fourth step adds `go test -race ./...`.
`-race` roughly doubles wall time; paying it on every PR push would slow the feedback loop for no benefit a merged commit doesn't already get, so it runs once, after merge, not per-push-to-a-PR-branch.

Both triggers set `paths-ignore: ["**.md", "spec/**"]` — a docs-only or spec-only change never burns a runner — and the workflow's `concurrency` group is `ci-${{ github.workflow }}-${{ github.ref }}` with `cancel-in-progress: true`, so pushing a second commit to the same PR cancels the first run's job rather than queuing behind it.

## Why `realinfra` is not in this workflow

`infra/`, `pipeline/`, and `security/` each carry a `//go:build realinfra` suite (`go test -tags realinfra ./...`).
Their own doc comments already scope them as a real/acceptance-machine suite ("needs a real machine", "on an acceptance machine run: ...") rather than a hosted-CI one.
`infra/`'s cases specifically need a working `nix` with network access to fetch the pinned nixpkgs — GitHub-hosted runners have docker but not nix, and wiring a nix installer plus trusting the nixpkgs substituter to behave predictably on every push is a real source of flakiness this pipeline does not take on for a suite explicitly designed to run by hand on a real machine first.
`security/`'s suite needs only `ssh-agent`/`ssh-add`/`ssh-keygen` (already present on GitHub-hosted runners) and `pipeline/`'s needs only docker + the Go toolchain — both are lighter, but are kept out of hosted CI alongside `infra/`'s for the same "acceptance machine, not CI" framing the suites already declare of themselves.
See `docs/deploy.md`'s CI section for the operator-facing version of this note.

## Nightly fuzz (`schedule`)

`.github/workflows/fuzz.yml` exists because `spec/reviews/2026-07-18.md` §5 calls for fuzzing the config/journal/extraction parsing functions on a recurring cadence, beyond what `go test` runs on every push.
It runs once nightly (`17 5 * * *`, off the top of the hour) and on `workflow_dispatch` for an ad hoc run.

**Targets are discovered, not enumerated.** The alternative — a YAML list of fuzz function names — goes stale the moment someone adds a `func Fuzz*` and forgets to update the workflow.
The job instead does:

```bash
grep -rl '^func Fuzz' --include='*_test.go' .        # which files define a fuzz target
grep -oE '^func (Fuzz[A-Za-z0-9_]+)' "$file"          # which function(s), per file
```

and for each `(package-dir, FuncName)` pair runs

```bash
go test -run='^$' -fuzz="^${name}\$" -fuzztime="${FUZZTIME}" "./${dir}"
```

`FUZZTIME` defaults to `3m` per target (overridable via `workflow_dispatch` input) — at the time this module was written, eight targets exist across three packages (`config`: `FuzzParseRef`, `FuzzRewriteStepRefs`, `FuzzConfigAssemble`; `failure`: `FuzzJournalLoad`, `FuzzInputHash`, `FuzzInputHashRawMessage`; `pipeline`: `FuzzExtractAdapt`, `FuzzParseItems`), so eight sequential 3-minute runs fit comfortably inside the job's 90-minute timeout.
A target added later is picked up the following night with no workflow edit.

A failing target leaves Go's usual `testdata/fuzz/<FuzzName>/<hash>` failing-input file in the tree; the job uploads every `**/testdata/fuzz/**` path as a `fuzz-failures` artifact on failure (`if: failure()`) so the crashing input can be pulled down and added as a seed corpus entry without reproducing the crash by hand first.

## Third-party action pinning

Every third-party action is SHA-pinned with a version comment (`actions/checkout@<sha> # v7.0.1`, etc.), not a floating major-version tag — a deliberate supply-chain-posture choice for this pipeline, made explicitly rather than left to whichever action version happened to be current when a workflow was first written.
