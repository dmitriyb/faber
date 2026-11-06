# Change Proposal: static inputs baked into the image — faber-box, hooks and skills as image content, tagged by their bytes

## Context

Three things the box needs before it runs are shared with it today as read-only
host bind mounts: the `faber-box` entry binary (`-v <host>/faber-box:/faber/bin/faber-box:ro`),
the template's hook scripts (`/faber/hooks/<slot>`) and its skills tree
(`/faber/skills`). On a docker host with userns-remap the box's user is a
different host uid (container 1000 is host 101000), so a read-only bind works only
when the host file happens to be world-readable, and a writable one does not work
at all. This is the first of four changes that stop the box from sharing any host
path; it takes the three static inputs, because they are static: they change when
the operator edits them, not per run, which is exactly what an image layer is for.

The image is built by Nix (`dockerTools.buildLayeredImage`) from a pinned package
list and an optional overlay, and tagged by a hash of exactly the inputs that
determine its content (`infra/image.go` `imageTag`: schema version, resolved pin,
sorted packages, overlay bytes). The build already stages the overlay's bytes
beside the rendered expression and imports them by relative path, reading the
file once so the tag hashes exactly what the build imports. The same discipline
extends to three more inputs without a new mechanism.

Settled with the operator: a faber upgrade or a hook or skill edit requiring
`faber build` is accepted; `on_failure` hooks run on the host and are not image
content; `box_bin` in host.json stays, now naming the binary to bake.

## Proposed change

- **The inputs layer (infra).** `renderImageExpr` adds one derivation to
  `contents`: `pkgs.runCommand "faber-inputs"` that places `/faber/bin/faber-box`,
  `/faber/hooks/<slot>` for each declared in-box slot (context, prelude, postlude)
  and `/faber/skills/…` (a named skills set farmed as `<name>/…`, or an inline
  root as is) into the image root, root-owned, executable bits kept. The files are
  staged beside `expr.nix` exactly as the overlay is and referenced as `./…` path
  literals; no user path is spliced into Nix source. `/faber` stays a plain
  directory so docker's `/faber/result` and `/faber/bundle` mount points keep
  working. No user, no entrypoint in the image config; the container command stays
  `/faber/bin/faber-box`.
- **The tag (infra).** `imageTag` folds, after the overlay: the faber-box bytes'
  sha256; per declared slot the pair (slot name, hook bytes' sha256); for skills
  the sorted (relative path, executable bit, sha256) of every regular file;
  `imageSchemaVersion` becomes `"2"`. `ImageTag` takes an `ImageInputs{BoxBin,
  Hooks, Skills}` beside the `BuildDef`; `ImageBuilder` is constructed with the
  faber-box path. The bytes are read once per tag or build (`readInputs`), and a
  tag is memoized for the lifetime of one `faber run`/`resume` through the
  `imageTagger` seam, so a run reads faber-box and the hooks once, not per node.
  `faber build` resolves hooks and skills with the same exported resolvers the
  desugarer uses (`config.ResolveHooks`, `config.ResolveSkills`), so the build-time
  tag equals the run-time tag by construction.
- **What `faber build` refuses (infra, config).** A faber-box that is not a
  linux/<image arch> ELF binary (magic, class, machine; a darwin build on a darwin
  host is the real mistake this catches), before anything is staged. A hook file
  that fails `config.CheckHookFile` (absent, not regular, not executable, no `#!`).
  A skills entry that is not a directory. Each refusal names the template and the
  path.
- **The mounts (agent, pipeline).** `BoxSpec` loses `EntryBinary`, `ContextHook`,
  `PreludeHook`, `PostludeHook`, `SkillsDir` and their binds; `AgentBoxes` and
  `Reentry` lose `EntryBinary`; the run-time skills staging (`stageSkills`,
  `copyTree`) moves to build time. `FABER_SKILLS_LINK` stays: the link is env, the
  tree is content. The box itself does not change: it reads `/faber/hooks/<slot>`
  and links `/faber/skills` exactly as before.
- **host.json `box_bin` (config, cmd).** Keeps its name and its absolute-path rule;
  its meaning becomes "the faber-box binary to bake" (default: next to the faber
  executable). `faber upgrade` keeps swapping the pair; after an upgrade every tag
  moves and the run preflight says "run `faber build` first", which is the point.
  The contract-version mismatch messages (`agent/box/env.go`, `agent/extract.go`)
  stop blaming `box_bin` and say "a stale image: run faber build".
- **Resume (failure).** Hook and skill edits now move the tag, so
  `checkImageDrift` refuses a resume after one, with `--fresh` as the escape, the
  way an overlay edit already does; the message names hooks and skills among the
  image inputs. The run-start hook check stays for `on_failure` and the three
  in-box slots (the build reads them anyway).
- **The workflow in the spec (delivery).** A short leaf, `arch_pr_workflow.md`,
  under the delivery module: spec first, code and tests, an independent
  fresh-context review of the whole diff before any commit, real-machine
  verification where a daemon is needed, one commit per pause, the PR opened by the
  operator. Asked for at the start of this release; recorded here, in the first PR
  of this train.
- **Out of scope.** Writable paths (the result dir, sessions, volumes, the agent
  socket): the next three proposals. Embedding faber-box into the faber binary.

## Impact expectation

- **infra**: the inputs derivation in the expression, `ImageInputs`, the widened
  tag, the ELF check, the build-time staging of hooks and skills, the manifest line;
  `arch_image_builder.md`, `impl_nix_build.md`, `arch_container_runner.md`,
  `impl_run_argv.md`, `flow_build_run.md`, `test_infra.md`; requirements "Nix image
  build" and "Container run primitive".
- **config**: `ResolveHooks`/`ResolveSkills` exported; `box_bin` wording;
  `arch_cli.md`, `impl_cli.md`, `arch_schema_types.md`, `arch_desugarer.md`,
  `impl_desugaring.md`, `arch_loader.md`.
- **agent**: `BoxSpec` and the mount list; the mismatch wording;
  `arch_phase_sequencer.md`, `impl_phase_sequencing.md`, `arch_prelude_hooks.md`,
  `impl_hook_result_contracts.md`, `arch_result_extractor.md`, `flow_box_execution.md`,
  `test_box_lifecycle.md`.
- **pipeline**: `AgentBoxes`/`Reentry` fields, run-time staging removed, tag
  memoization honoured by the seam; `impl_scheduling.md`, `arch_scheduler.md`,
  `test_pipeline_execution.md`; the realinfra round trip runs a nix-built image.
- **failure**: `arch_recovery_modes.md`, `arch_journal.md` (image inputs).
- **delivery**: `arch_release.md` (faber-box is shipped beside faber to be baked,
  not mounted), `arch_pr_workflow.md`, `module.json`.
- **cmd**: the builder takes the faber-box path; `imageTagger` memoizes.
- **docs**: `commands.md`, `install.md`, `deploy.md`, `configuration.md`.
- **spec/project.json**: the untrusted-box requirement's "pure function of the
  toolset" becomes "of the toolset and the engine's own inputs".
