# Test section: Infra integration tests

Scenarios spanning ExecAdapters, ImageBuilder, and ContainerRunner — the path
from a template's `build:` section to a finished container run. Unit tests
live beside the code; these are the module-level behaviors that must hold.
Fake-backed scenarios run in `go test ./...`; scenarios needing real `nix` and
`docker` are build-tag-guarded (`//go:build realinfra`) and run in the
acceptance environment only.

## Fixtures

- The four reference templates' `build:` sections from
  `spec/test_reference_workflows.md` (packages + shared overlay).
- A recording fake per adapter interface: appends argv, pops canned results.
- A minimal real toolset (`[coreutils]`, no overlay) for the integration path.

## Scenarios

1. **Golden argv.** A fully populated RunSpec (limits, every engine mount kind —
   a result bind rw, `:ro` hook + entry binds, a `:ro` `/faber/skills` bind, a
   `/workspace` volume, a `/tmp` tmpfs — three env keys, a `StdinSecrets`
   payload, a representative binding fragment containing
   `--network`, an agent-socket `-v`, `SSH_AUTH_SOCK`, proxy env, a
   `--tmpfs /run/secrets`, and `--runtime`) assembles to the golden argv
   byte-for-byte: fixed section order, `-i` immediately after `--rm` (because
   `StdinSecrets` is set), a bind as `-v Host:Container[:ro]`, a volume as bare
   `-v Container`, a tmpfs as `--tmpfs Container`, sorted env, the fragment
   contiguous and verbatim at its slot, image then entry argv last. The same
   spec with `StdinSecrets` empty assembles identically except no `-i` appears.
2. **Argv discipline (property).** Over randomized RunSpecs (fuzzed env,
   mounts, fragments, `StdinSecrets`), the assembled argv never contains a
   docker-socket mount, `--privileged`, `--user`, `--network=host`, or any
   `-v`/`--tmpfs` not present in the spec's mounts or fragment; `--rm` and
   `--name` are always present; declared memory/cpus always surface as flags,
   absent resources emit none; `-i` appears exactly when `StdinSecrets` is
   non-empty and never otherwise, and the token bytes never appear anywhere in
   the argv. A RunSpec with no skills mount emits no `/faber/skills` `-v`; one
   with it emits the bind exactly once, always `:ro`.
3. **Tag determinism and pin resolution.** Same packages shuffled → identical
   tag; one package added, one overlay byte changed, or the default pin bumped →
   different tag. Two builds identical except for a per-build `pin` produce
   **different** tags (the resolved pin folds into the hash); a build with **no**
   `pin` resolves to the compiled-in `defaultPin` and produces the **same** tag as
   today (goldens byte-stable), and its rendered expression fetches the default
   snapshot. The tag is computed with zero adapter calls (pure, no nix).
3b. **Run-time tag equals build-time tag across the `imageTagger` seam.** For a
   **pinned** toolset, the tag `ImageBuilder.ImageTag` computes from the resolved
   `BuildDef` (the `faber build` path) equals the tag recomputed from the derived
   `ResolvedTemplate` via the `imageTagger` reconstruction
   (`BuildDef{Packages, Overlay, Pin: template.Pin}` — the run/resume path). This is
   the regression that catches a dropped `template.Pin`: without the pin the
   reconstruction falls back to `defaultPin` and the two tags diverge. A **pin-less**
   template's two tags likewise match (nil → default both times) — necessary but not
   sufficient, which is why the pinned case is the one that must be asserted.
4. **Build skip and single-flight.** Fake docker reports the tag exists →
   `Build` returns the tag with no `NixClient.Build` call. Two goroutines
   building the same tag through the per-tag lock produce exactly one nix
   invocation.
5. **Proof failure shape.** Fake NixClient returns `{"git":true,
   "item-tracker-cli":false, "spec-mapper-cli":false}`: `ProvePackages`
   returns a joined error naming the template and *both* packages, and no
   build was attempted. An eval crash yields one error against the overlay
   path.
6. **Exec error contract.** A failing adapter call surfaces `*ExecError` via
   `errors.As` with exit code and bounded stderr tail; for `CommandRunner`
   the error text and log records contain the command path but no argv and
   no stdout bytes (secret hygiene).
7. **Structured parse catalog.** Recorded real outputs of `docker image
   inspect --format '{{json .Id}}'`, `docker load`, `nix build --json`, and
   `nix eval --json` (testdata fixtures) parse to the expected values;
   truncated/malformed fixtures produce wrapped parse errors, never a
   scraping fallback.
8. **Build-load-run round trip** (integration). The minimal toolset builds
   via real nix, loads with the computed tag, and `ContainerRunner.Run` with
   entry `["ls", "/bin"]` returns exit 0 with output captured; store paths
   inside the container are root-owned and not writable by the run user;
   rerunning `Build` performs no nix invocation (daemon-as-cache).
9. **Kill on cancel** (integration). A container running `sleep 300` has its
   context cancelled: `Run` returns `context.Canceled` within the grace
   window, `docker ps -a` no longer lists the deterministic name (`--rm`
   completed after kill), and the RunResult still carries partial output and
   timing.
10. **Non-zero exit is data.** Entry `["false"]` (fake and integration):
    `Run` returns `err == nil`, `ExitCode == 1`, output attached — the
    classification boundary belongs to the failure module.
11. **Stdin secrets delivery.** With a `StdinSecrets` payload, the fake
    `DockerClient.ContainerRun` records a non-nil stdin reader whose full
    contents equal the payload bytes and observes EOF; the payload is never
    logged and never appears in the recorded argv. With empty `StdinSecrets`,
    the recorded stdin reader is nil and no `-i` is in the argv. (Integration:
    entry `["cat", "/dev/stdin"]` echoes the exact payload bytes back through
    captured output, proving the byte stream reached the container intact.)

## Edge cases

- Empty binding fragment: argv is still valid (a bare engine-only run, e.g.
  `faber build` smoke-testing an image).
- Empty entry argv: image default entrypoint runs; assembly appends nothing
  after the tag.
- Output beyond the 256 KiB cap: RunResult holds the tail, head discarded,
  no allocation blow-up on a log-spamming box.
- Package name failing the `[A-Za-z0-9._-]+` charset check: rejected before
  any expression is rendered, error names the template and the offending name.
- Loaded tag disagreeing with the computed tag: `Build` fails loudly rather
  than returning either tag.
