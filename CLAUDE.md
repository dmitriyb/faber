# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Faber is a generic containerized-agent workflow engine. Given an `orchestrator.yaml`, it desugars the declarative workflow into an acyclic JSON IR, builds immutable agent images from pinned toolsets via Nix, and executes the workflow as a host-side DAG of single-purpose containers ("boxes") — with security bindings enforced from outside every container, pluggable metering, and journal-based resume.

Faber is **mechanism, not policy**: it knows `docker build`/`docker run`, a workflow DAG, and pluggable credential/metering interfaces. It never learns domain words (issue trackers, gate services, spec tools). All opinionated behavior arrives as user config: opaque scripts, params, data-source commands, and companion services on a named docker network.

## Module Hierarchy

| Module | Purpose | Depends On |
|--------|---------|------------|
| config | orchestrator.yaml schema, typed params, desugaring to JSON IR, validation, CLI, logging | — |
| infra | Typed subprocess actuation (docker/git/nix), Nix image build, container run primitive | config |
| security | Run-time bindings: network (egress lock), remote (pinned gateway), identity (ephemeral ssh-agent), credential delegation | config, infra |
| agent | The box: fixed phase order context → prelude → agent → result, structured result extraction | config, infra, security |
| metering | Pluggable estimate/actual budget hooks, endpoint fidelity tiers, 429-defer floor | config |
| failure | Structured step results, on_failure cleanup, retry, run journal, resume/recovery modes | config |
| pipeline | IR executor: topological + parallel step scheduling, CEL conditions, generate expansion | config, infra, security, agent, metering, failure |

## Spec Convention (spexmachina)

The spec is a typed graph managed by [spexmachina](https://github.com/dmitriyb/spexmachina):

- `spec/project.json` — project requirements, module declarations, milestones, test plan
- `spec/<module>/module.json` — module requirements (traced to project requirements via `preq_id`), components (`implements` requirements), impl sections and test sections (`describes` components), data flows
- `spec/<module>/{arch,impl,flow,test}_*.md` — content leaves referenced from `module.json`
- `spec/proposals/YYYY-MM-DD-*.md` — every change starts with a proposal

IDs are 12-char hex identity hashes (`spex hash-id`), never assigned manually. Validate with `spex validate`; a change session must end with `spex diff --json` reporting `errors: []`.

Requirements titled `Deferred: …` are captured backlog (the design's open edge cases) — they are part of the spec so they are not lost, but are explicitly out of scope for the first implementation pass.

## Technical Constraints

- **Go standard library first**: only external deps are `yaml.v3`, `cel-go`, and `x/term`
- **CLI actuation behind typed interfaces**: docker, git, nix, and user resolvers are invoked via `os/exec` behind clean interfaces with structured (JSON) I/O — never stdout-scraping
- **YAML in, JSON IR out**: humans author YAML; the engine executes only the desugared, validated JSON IR
- **Validate before run**: wiring, type, and cycle errors surface at `faber validate`, never mid-run
- **Untrusted box**: every real control is enforced from outside the container or is immutable relative to it; no secret is ever materialized inside a container (handles only)
- **No global state**: config passed explicitly, loggers created per component

## Build & Test

- Build: `go build ./...`
- Test: `go test ./...`
- Vet: `go vet ./...`

## Git Conventions

- Default branch is `main` (never `master`)
- Always `git fetch origin` before creating a new branch
- Always branch from `origin/main`, not from the current branch
- Commits must be SSH-signed; never bypass signing

## Organizational Constraints

- **Module dependency order**: config → infra → security → agent → metering/failure → pipeline, implemented in that order
- **Spec traceability**: all code must trace back to spec requirements; tests verify requirements, not just coverage
- **Mechanism/policy split**: any PR that teaches faber a domain word (a tracker, a gate service, a spec tool) is wrong by construction — find the config seam instead
