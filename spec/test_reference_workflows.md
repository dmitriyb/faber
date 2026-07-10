# Test Scenario: Reference workflows end-to-end

The acceptance example the engine must run. This single `orchestrator.yaml` is the
completeness test for the whole design: it is the dogfooded shell harness translated
into pure config. Every domain-specific behavior reduces to an opaque script, a
param name, or a data-source command — faber itself never learns what an "item",
a "gateway", or a "reviewer" is beyond the config values.

## The reference orchestrator.yaml

```yaml
version: 1

# Workflow-level bindings: the wiring seam is a named docker network.
# Companion services (gateway, egress proxy, token proxy) are brought up
# out-of-band by the user's docker-compose; faber attaches step containers
# to the network and treats everything on it as opaque.
network:
  name: agents-internal            # docker network create --internal agents-internal
  proxy: http://egress:8888        # injected as HTTPS_PROXY / HTTP_PROXY in every box
  no_proxy: [gateway, localhost, 127.0.0.1]

remote:
  url: ssh://git@gateway/srv/git   # the box's only reachable git remote (repo name appended)
  host_key_file: ./keys/gateway_host_key.pub   # pinned => StrictHostKeyChecking=yes
  # tofu: true                     # sandbox-only alternative, explicit opt-in

credentials:
  resolver: ./hooks/get-token      # get_token(service): opaque user command, host-side
  services:
    agent-api:
      mode: file                   # degraded raw-token mode: ro tmpfs file mount, shredded after run
      # mode: proxy                # preferred: unauthenticated local endpoint, proxy injects auth
      # endpoint: http://token-proxy:8402

identities:
  implementer: {key: ./keys/implementer}   # resolver-supplied key material; one key per box
  reviewer:    {key: ./keys/reviewer}
  merger:      {key: ./keys/merger}

templates:
  implement:
    build:
      packages: [git, openssh, go, gopls, claude-code, item-tracker-cli, spec-mapper-cli]
      overlay: ./nix/overlay.nix   # user derivations for CLIs not in nixpkgs
    run:
      identity: implementer
      resources: {memory: 8g, cpus: 4}
    skill: implement               # the agent skill invoked headlessly in the box
    hooks:
      context: ./hooks/gather-context     # opaque: resolve item -> context bundle
      prelude: ./hooks/claim-item         # opaque: branch + claim, signed
      on_failure: ./hooks/release-item    # opaque: undo the claim, delete orphan branch
    inputs:
      repo: {type: string, required: true}
      item: {type: string, required: true}
    output:
      branch: {type: string, required: true}
      pr:     {type: int,    required: true}

  review:
    build:
      packages: [git, openssh, go, claude-code, item-tracker-cli, spec-mapper-cli]
      overlay: ./nix/overlay.nix
    run:
      identity: reviewer
    skill: review
    hooks:
      context: ./hooks/fetch-pr
    inputs:
      repo: {type: string, required: true}
      pr:   {type: int,    required: true}
    output:
      verdict: {type: string, enum: [approved, changes], required: true}

  fix:
    build:
      packages: [git, openssh, go, gopls, claude-code, item-tracker-cli, spec-mapper-cli]
      overlay: ./nix/overlay.nix
    run:
      identity: implementer
    skill: fix
    hooks:
      context: ./hooks/fetch-pr
    inputs:
      repo: {type: string, required: true}
      pr:   {type: int,    required: true}
    output:
      status: {type: string, required: true}

  merge:
    build:
      packages: [git, openssh]
    run:
      identity: merger
    skill: merge
    inputs:
      repo: {type: string, required: true}
      pr:   {type: int,    required: true}
    output:
      merged: {type: bool, required: true}

workflows:
  # One work item: implement -> (review; fix)* until approved -> merge.
  task:
    params:
      repo: {type: string, required: true}
      item: {type: string, required: true}
    steps:
      - id: implement
        use: implement
        with: {repo: ${params.repo}, item: ${params.item}}

      - id: review-cycle
        loop:
          max: 3                                       # bound; exhaustion = failure
          until: steps.review.verdict == "approved"    # CEL over the iteration's results
          steps:
            - id: review
              use: review
              with: {repo: ${params.repo}, pr: ${steps.implement.pr}}
            - id: fix
              use: fix
              when: steps.review.verdict == "changes"
              with: {repo: ${params.repo}, pr: ${steps.implement.pr}}

      - id: merge
        use: merge
        when: steps.review.verdict == "approved"       # post-loop ref = final executed iteration
        with: {repo: ${params.repo}, pr: ${steps.implement.pr}}

  # A group of work items: fan the task workflow out over a data source.
  epic:
    params:
      repo: {type: string, required: true}
      group: {type: string, required: true}
    sources:
      members:
        command: ./hooks/list-members       # opaque; contract: {"items":[{"id":..,"deps":[..]}]}
        args: ["${params.group}"]
    steps:
      - id: tasks
        generate:
          source: members
          workflow: task
          with: {repo: ${params.repo}, item: ${item.id}}
          # inter-instance edges derived from each item's deps
```

Notes on fidelity to the proven harness:

- The templates mirror the harness's skill/role table: implement/fix run under the
  implementer identity, review under the reviewer, merge under a merger. Role
  *enforcement* (fingerprint -> role, content rules, approval re-check) belongs to
  the user's gateway; faber only guarantees one key per box.
- The harness's epic ran as one mega-box with an in-session fan-out; faber lifts the
  fan-out to the host DAG (`generate`), so every item gets its own single-identity
  boxes and its own PR. The design's "merge after" is therefore realized per item:
  each task instance settles its review loop and then merges its own PR under the
  merger identity. This resolves the plan's open "merger" question in favor of
  merge-as-a-real-step.
- The data-source contract `{items: [{id, deps}]}` is the harness's tracker query
  (members-of-group plus dependency edges) with the tracker-specific parts hidden
  behind `./hooks/list-members`.

## Setup

- Companion services up out-of-band: `docker compose up` for the gateway and egress
  proxy, both dual-homed on `agents-internal` + the default network; the gateway
  holds the forge credential; the proxy allow-lists only the agent API endpoint.
- A sandbox repo registered at the gateway with signed-commit enforcement, a
  fingerprint->role map covering the three identity keys, and a role-content rule
  (e.g. item-state transitions restricted to the reviewer key).
- `faber validate orchestrator.yaml` passes; `faber build` produces the four images
  via Nix.

## Scenarios

1. **Task happy path.** `faber run task --param repo=sandbox --param item=I-1`:
   implement box clones from the gateway, signs with the forwarded implementer key,
   pushes; the gateway accepts and auto-opens a PR; review emits
   `{verdict: approved}` on iteration 1; fix is skipped (condition false); merge
   runs under the merger key; the run report shows ok/ok/skipped/ok and the PR is
   merged and Verified upstream. Assert: no container ever held the forge
   credential; each box's ssh-agent held exactly one key.
2. **Review loop settles on iteration 2.** Review 1 emits `changes` (status ok —
   an unfavorable verdict is not a failure); fix 1 runs; review 2 emits `approved`;
   the unrolled chain terminates early (iteration 3 skipped); merge runs.
3. **Loop exhaustion.** Three `changes` verdicts: the loop's `max` is reached, the
   review-cycle settles as failed, merge is skipped (dependency failed), the run
   report says which bound was exhausted.
4. **Epic fan-out.** `faber run epic --param repo=sandbox --param group=G-1` with
   `list-members` emitting three items where `I-3.deps = [I-1, I-2]`: task
   instances for I-1 and I-2 run concurrently, I-3 starts only after both settle;
   each instance lands and merges its own PR.
5. **Desugared IR golden file.** `faber validate --emit-ir` on this file matches
   the committed golden IR byte-for-byte (loop unrolled to three conditional
   iterations plus the post-loop selector; generate node carrying the source ref
   and sub-workflow ref).

## Edge cases

- `list-members` emits `{items: []}`: the generate node is a no-op, the epic run
  report shows zero instances, exit 0.
- `list-members` emits malformed JSON: the generate node fails with the
  data-source contract error; nothing was launched.
- A `with:` binding referencing `${steps.implement.branch}` typo'd as
  `${steps.implement.branches}` fails `faber validate`, not the run.
