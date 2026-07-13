# Test section: Infra integration tests

Scenarios spanning ExecAdapters, ImageBuilder, and ContainerRunner — the path
from a template's `build:` section to a finished container run. Unit tests
live beside the code; these are the module-level behaviors that must hold.
Fake-backed scenarios run in `go test ./...`; scenarios needing real `nix` and
`docker` are build-tag-guarded (`//go:build infra_integration`) and run in the
acceptance environment only.

## Fixtures

- The four reference templates' `build:` sections from
  `spec/test_reference_workflows.md` (packages + shared overlay).
- A recording fake per adapter interface: appends argv, pops canned results.
- A minimal real toolset (`[coreutils]`, no overlay) for the integration path.

## Scenarios

1. **Golden argv.** A fully populated RunSpec (limits, every engine mount kind —
   a result bind rw, `:ro` hook + entry binds, a `/workspace` volume, a `/tmp`
   tmpfs — three env keys, a representative binding fragment containing
   `--network`, an agent-socket `-v`, `SSH_AUTH_SOCK`, proxy env, a `:ro` secret
   mount, and `--runtime`) assembles to the golden argv byte-for-byte: fixed
   section order, a bind as `-v Host:Container[:ro]`, a volume as bare
   `-v Container`, a tmpfs as `--tmpfs Container`, sorted env, the fragment
   contiguous and verbatim at its slot, image then entry argv last.
2. **Argv discipline (property).** Over randomized RunSpecs (fuzzed env,
   mounts, fragments), the assembled argv never contains a docker-socket
   mount, `--privileged`, `--user`, `--network=host`, or any `-v`/`--tmpfs` not
   present in the spec's mounts or fragment; `--rm` and `--name` are always
   present; declared memory/cpus always surface as flags, absent resources emit
   none.
3. **Tag determinism.** Same packages shuffled → identical tag; one package
   added, one overlay byte changed, or the pin rev bumped → different tag;
   the tag is computed with zero adapter calls (pure, no nix).
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
