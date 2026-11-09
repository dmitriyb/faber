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

1. **Golden argv.** A fully populated ContainerSpec (limits, every engine mount kind —
   a template bind rw, a `/workspace` volume, a `/faber/result` and a `/tmp` tmpfs — three env keys, a `StdinSecrets`
   payload, a representative binding fragment containing
   `--network`, a `--tmpfs /faber/agent`, `SSH_AUTH_SOCK`, proxy env, a
   `--tmpfs /run/secrets`, and `--runtime`) assembles to the golden argv
   byte-for-byte: fixed section order, `-i` immediately after `--rm` (because
   `StdinSecrets` is set), a bind as `-v Host:Container[:ro]`, a volume as bare
   `-v Container`, a tmpfs as `--tmpfs Container`, sorted env, the fragment
   contiguous and verbatim at its slot, image then entry argv last. The same
   spec with `StdinSecrets` empty assembles identically except no `-i` appears.
2. **Argv discipline (property).** Over randomized ContainerSpecs (fuzzed env,
   mounts, fragments, `StdinSecrets`), the assembled argv never contains a
   docker-socket mount, `--privileged`, `--user`, `--network=host`, or any
   `-v`/`--tmpfs` not present in the spec's mounts or fragment; `--rm` and
   `--name` are always present; declared memory/cpus always surface as flags,
   absent resources emit none; `-i` appears exactly when `StdinSecrets` is
   non-empty and never otherwise, and the token bytes never appear anywhere in
   the argv. No ContainerSpec emits a `/faber/skills`, `/faber/hooks` or `/faber/bin` `-v`:
   those paths are image content; and no spec the agent module assembles
   emits a `/faber/result` `-v` — the result dir is a `--tmpfs`.
3. **Tag determinism and pin resolution.** Same packages shuffled → identical
   tag; one package added, one overlay byte changed, or the default pin bumped →
   different tag. Two builds identical except for a per-build `pin` produce
   **different** tags (the resolved pin folds into the hash); a build with **no**
   `pin` resolves to the compiled-in `defaultPin` and produces the **same** tag as
   today (goldens byte-stable), and its rendered expression fetches the default
   snapshot. The tag is computed with zero adapter calls (pure, no nix). One
   changed byte in a declared hook, one changed skill file, or a different
   faber-box binary → different tag; the same hook and skill bytes read from
   different paths → identical tag (bytes, not paths); a template with no skills
   leg and no hooks still folds the faber-box bytes. `faber build` refuses,
   before staging, a faber-box that is not a linux ELF of the image's
   architecture, a hook failing the hook-file check, and a skills source that
   is not a directory, naming the template and the path; the rendered
   expression carries the `faber-inputs` derivation with one `install` line per
   declared slot and the skills copy only when a leg is declared.
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
   entry `["ls", "/bin"]` returns exit 0 with the listing in the `Stdout`
   writer and nothing in the captured stderr — the two streams reach two
   writers; store paths
   inside the container are root-owned and not writable by the run user;
   rerunning `Build` performs no nix invocation (daemon-as-cache).
9. **Kill on cancel** (integration). A container printing without end on
   its stdout (`yes`), captured through a bounded `Stdout` writer, has its
   context cancelled: `Run` returns `context.Canceled` within the grace
   window, `docker ps -a` no longer lists the deterministic name (`--rm`
   completed after kill), the partial output reached the writer, and the
   ContainerResult still carries timing.
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

12. **Two writers.** With `ContainerSpec.Output` and `Stdout` set, the fake
    docker's stderr bytes reach `Output` in full and byte-identical to the
    bounded tail, its stdout bytes reach `Stdout` in full, and neither writer
    sees the other's bytes; with `Stdout` nil the stdout bytes are discarded
    and `Output` is unchanged.
13. **Exec argv.** The fake-backed `Exec` records `exec -i <name> <argv…>`
    with a non-nil stdin and `exec <name> <argv…>` without one; `ExecTerminal`
    records `exec -it -u <user> -w <dir> -e K=V <name> <argv…>` with the env
    sorted. A non-zero exit from `Exec` is data (`code, nil`).
14. **Enter choreography.** With a fake docker, `Enter` issues, in order:
    remove by name, `run --rm -d --name <name> … <image> /faber/bin/faber-box
    hold` (no `-i`), `exec -i <name> /faber/bin/faber-box receive` with the
    caller's input stream drained to EOF, `exec -it -u <user> … <name>
    <entry>`, kill by name — and kill still runs when the receive exits
    non-zero (the attach never happens, the error names the receive) or the
    attach fails. (Integration: a held box of a built image receives a
    stream over `docker exec` into its handoff tmpfs and its session dir
    under HOME, and a second exec finds every file, HOME, the path down to
    the session dir, /workspace and /tmp owned by the run user.)
15. **Attach lifecycle.** `Run` with `ContainerSpec.Attach` set calls it
    with the container's name while the container runs (the fake run
    blocks until the attach has been observed) and cancels the attach's
    context once the run returns, waiting for it; an attach error is logged,
    never returned, and a spec without one never starts a goroutine. `Enter`
    starts it after the held box is up and before the receive, and cancels
    it before the kill. `parseJSONBool` decodes the inspect state's boolean
    and refuses anything else; `ExecWhenRunning` against a fake that reports
    not-running for its first polls calls `Exec` exactly once, after a true
    `Running`, and a context that ends while polling returns its error with
    no exec. (Integration: `Running` is false for an absent container and
    true for a held box, in the structured-queries and hold-and-receive
    tests.)

## Edge cases

- Empty binding fragment: argv is still valid (a bare engine-only run, e.g.
  `faber build` smoke-testing an image).
- Empty entry argv: image default entrypoint runs; assembly appends nothing
  after the tag.
- Output beyond the 256 KiB cap: ContainerResult holds the tail, head discarded,
  no allocation blow-up on a log-spamming box. The stdout stream is never
  held in the runner at all.
- Package name failing the `[A-Za-z0-9._-]+` charset check: rejected before
  any expression is rendered, error names the template and the offending name.
- Loaded tag disagreeing with the computed tag: `Build` fails loudly rather
  than returning either tag.
