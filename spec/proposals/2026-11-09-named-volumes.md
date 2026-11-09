# Change Proposal: template volumes as named docker volumes, the check remote as a volume seeded from a bundle, `faber volumes`, and the v0.8.0 release notes

## Context

After the agent relay (#33) a box shares two host paths left: a template's
`run.volumes` entries, which are host directories bind mounted into the box
(`-v /host/path:/container/path`), and the check run's throwaway remote, a
host directory of bare repositories bind mounted read-write at
`/faber/remote` so the hooks' pushes land where the check can look. Under
userns-remap both are unwritable to the box's uid, and a world-writable host
directory is the only workaround. Settled with the operator for v0.8.0:
template volumes become docker named volumes, host paths are refused with no
migration shim, the check remote becomes a named volume faber seeds from a
git bundle through the box binary, a `faber volumes` command lists and prunes
a project's volumes, and the release notes close the train.

## Proposed change

- **Schema (config).** `run.volumes: {<name>: /container/path}`. The key is
  a docker volume name, `^[A-Za-z0-9][A-Za-z0-9_.-]*$`; a key starting with
  `/`, `.` or `~` is refused at validate as "host paths are not mounted;
  declare a named volume". The container path must be absolute, carry no
  `:`, and stay off the reserved paths, as today. Desugar passes the map
  through unchanged. `config.ProjectID(configPath)` is the first 12 hex
  digits of the SHA-256 of the config file's identity (symlinks resolved,
  absolute), and `config.VolumeName(project, name)` is
  `faber-<project>-<name>`: every run of one project shares its volumes, two
  projects never collide by accident, and the prefix is what `faber
  volumes` filters by.
- **The mount (infra, agent).** `infra.Mount` gains `KindNamed`, emitted as
  `-v <Name>:<Container>[:ro]`; `DockerClient` gains `VolumeList(ctx,
  prefix)` (`docker volume ls --filter name=<prefix> --format '{{json
  .Name}}'`, the daemon's filter narrowing and the exact prefix checked on
  the result) and `VolumeRemove(ctx, name)` (`docker volume rm`, an absent
  volume being success). `BuildContainerSpec` takes the project id
  (`BoxSpec.Project`) and emits a named mount per template volume, plus
  `FABER_VOLUMES`, the colon-separated container paths, so the preamble can
  chown each mount point to the run user (non-recursive): docker creates a
  volume on first use with the image's ownership of that path, root. The
  relative-bind floor in `Run` stays for the engine's own sake but no caller
  emits a bind any more. The git object cache paragraph of the phase
  sequencer names a named volume instead of a bound host directory;
  `FABER_GIT_CACHE` is unchanged.
- **The check remote (security, agent).** `checkRemoteBinding` contributes
  `-v faber-check-<id>-remote:/faber/remote` and the same `FABER_REMOTE_URL`
  as today. On a repository's first use (under the binding's mutex, as the
  bare clone was) it seeds the volume through a short container of the
  step's own image: `faber-box seed-remote <repo>` with the volume mounted
  and `git bundle create - --all` of the working directory's repository on
  its stdin. The verb reads the bundle to a file under the volume, runs the
  image's `git clone --bare` into a temporary directory and renames it to
  `<repo>.git` (a rename onto an existing repository fails, so two seeders
  cannot interleave and a present repository is left alone), chowns the
  tree to the run user and removes the bundle. `StepSpec` gains `Image`,
  which the pipeline and re-entry fill from the attempt's tag, and the
  binding takes the container runner as its seam, so the one docker-run
  assembly point stays infra's. The sandbox removes the volume when it
  closes unless `--keep`, which prints its name beside the directory.
- **`faber volumes` (config, cmd).** `faber volumes [--config path]
  [--json]` lists this project's volumes, template name and docker name;
  `faber volumes prune [--config path]` removes them, refused while any run
  in the project's store is live (the runs audit). A `VolumeController`
  seam carries the two docker verbs into the config CLI; the prefix logic
  is config's.
- **Release notes (docs, delivery).** `docs/release-notes/v0.8.0.md`: the
  train in operator terms — inputs baked into the image (`faber build` after
  an upgrade or a hook or skill edit; resume refuses after a hook edit,
  `--fresh`), outputs over the box's stdout and `--sessions` at step end,
  the agent over the relay and `agent_socket_group` retired, template
  volumes as named volumes with host paths refused, the check remote as a
  volume, `faber volumes`, contract v3 and what it refuses. The release
  leaf says the file is the release body for the tag.
- **Tests.** config: the volumes shape, the three refusals (host path,
  bad name, reserved path), `ProjectID` stable across path spellings and
  distinct across files, the `faber volumes` listing and prune (prune
  refused with a live run) against a fake controller. infra: the named
  mount in the golden argv, the two volume verbs' argv and parsing through
  the fake, the relative-bind floor unchanged. agent: the named mount and
  `FABER_VOLUMES` in the run spec, the preamble's chown list, `seed-remote`
  against a real local git repository (bundle in, bare repository out,
  idempotent, the race's rename). security: the check binding emits the
  volume token and seeds once per repository through a recorded container
  spec (image, mount, entry, stdin bytes); a present repository is not
  seeded again; a step without a repository in the working directory still
  fails before launch. realinfra: a named volume written by one box is read
  by the next and removed by `VolumeRemove`; `seed-remote` into a volume on
  a nix image with git, then a clone from it as the run user.

## Impact expectation

- **config**: requirements "Schema types" (volumes shape), "Loader"
  (refusals), "CLI commands" (`faber volumes`); components SchemaTypes,
  Loader, CLI; leaves `arch_schema_types.md`, `impl_schema_structs.md`,
  `arch_loader.md`, `arch_desugarer.md`, `arch_cli.md`, `impl_cli.md`.
- **infra**: requirements "Container run primitive" (the named mount) and
  "Typed subprocess interfaces" (volume verbs); components ContainerRunner,
  ExecAdapters; leaves `impl_run_argv.md`, `arch_exec_adapters.md`,
  `impl_exec_adapters.md`, `test_infra.md`.
- **agent**: requirement "Fixed box phase order" (the preamble's chown of
  the volume mount points, the seed verb); component PhaseSequencer; leaves
  `arch_phase_sequencer.md`, `impl_phase_sequencing.md`,
  `test_box_lifecycle.md`, the run-spec assembler's leaf.
- **security**: requirement "Check sandbox", component BindingSet; leaves
  `arch_binding_set.md`, `impl_bindings.md`, `test_security_bindings.md`.
- **pipeline**: `impl_scheduling.md` (the project id on the box spec, the
  image on the step spec).
- **delivery**: `arch_release.md` (the release body).
- **docs**: `configuration.md` (a volumes example), `commands.md` (`faber
  volumes`, the check row), `deploy.md`, `release-notes/v0.8.0.md`.
- **project**: the security requirement's wording on bindings.
