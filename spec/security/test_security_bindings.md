# Test section: Security binding tests

Integration scenarios spanning NetworkBinding, RemoteBinding,
IdentityBinding, CredentialBroker, and BindingSet — the path from resolved
step config to an assembled argv fragment and its guaranteed teardown.
(Unit tests live beside the code; these are the module-level behaviors that
must hold.) Docker and the resolver are faked via infra's typed adapters;
the ssh-agent scenarios run against a real agent binary where available and
skip otherwise.

## Fixtures

- The reference orchestrator.yaml's `network`/`remote`/`credentials`/
  `identities` sections, resolved into `StepSpec`s for the implement and
  merge steps.
- A fake resolver script emitting a fixed token; a failing variant (exit 1);
  a slow variant (context-cancellation tests).
- Throwaway file keys per identity; a scratch dir for agent sockets.

## Scenarios

1. **Golden fragment.** Assembling the implement step yields the exact
   ordered fragment — network flags, proxy env, NO_PROXY list, remote URL
   env with the repo param spliced (`.../sandbox.git`), pinned host-key
   env, socket mount + SSH_AUTH_SOCK, service handle env, no runtime flag —
   byte-identical across repeated assembly. With `agent-api` in file mode
   the fragment also carries exactly one `--tmpfs /run/secrets` and **no**
   token flag; `Assembled.SecretsStdin` decodes to `{"agent-api":
   "<base64(token)>"}`. Adding `runtime: runsc` appends exactly
   `--runtime=runsc` at the end and changes nothing else.
2. **One key per box.** After Prepare, the step's agent socket answers
   with exactly one fingerprint — the implement step's identity, not any
   other configured identity. Two steps prepared concurrently under
   different identities get distinct sockets, each seeing only its own
   key. A resolver that loads two keys produces the >1-key warning with
   both fingerprints; zero keys fails the step.
3. **Teardown always runs.** For each of: container exit 0, container
   exit non-zero, context cancelled mid-run, and a later binding's
   Prepare failing (failing resolver after the agent was spawned) — the
   agent process is gone and the socket directory is removed. File mode
   contributes no teardown and writes no host file, so there is nothing to
   shred; the test asserts no host path under the run's scratch area ever
   held the token and that `Assembled.Teardown` for a file-mode-only step
   is the identity/network unwind alone. The Prepare-failure case
   additionally asserts no container was ever started.
4. **Host-key modes.** Pinned: the fragment carries the key file's single
   line and nothing tofu-related; an unreadable or empty key file fails
   Prepare naming the remote binding. Tofu: the flag env appears and no
   key line does. Repo-less step: the remote binding contributes nothing
   at all. (Both-modes-configured is a loader rejection, covered in
   config's test catalog; asserted here only as "assembly never sees it".)
5. **File-mode secret containment.** With `agent-api` in file mode: the
   resolver ran host-side exactly once; the fragment contains exactly one
   `--tmpfs /run/secrets` and **no** `-v` mount and **no** env var carrying
   the token; `Assembled.SecretsStdin` is a single JSON object whose
   `agent-api` value base64-decodes to the resolver's token; no host file
   is written anywhere. A second file-mode service (`other-api`) still
   yields exactly one `--tmpfs /run/secrets` and a two-key payload with
   sorted keys, byte-identical across repeated assembly. Switching the
   service to proxy mode invokes no resolver, emits only the base-URL env,
   and leaves `SecretsStdin` nil (no `--tmpfs`).
6. **Secret redaction.** A `Secret` formatted via `%s`, `%v`, `%+v`,
   `fmt.Errorf` wrapping, and `json.Marshal` yields `[redacted]` in every
   case; a resolver failure's error string contains the service name and
   no stdout content.
7. **Unwinding order.** With instrumented bindings recording call order:
   setup runs network → remote → identity → credentials → runtime;
   teardown runs strictly reversed; a teardown error in the credentials
   hook does not prevent the identity teardown, and both errors surface
   joined.
8. **nftables mode.** A network def in nftables mode yields
   `--cap-add NET_ADMIN` with no proxy env; the missing-network preflight
   still applies and fails Prepare with the network name when the fake
   docker reports no such network.

9. **Registry round-trip + idempotency.** `add-key` on a fresh (missing)
   registry creates `roles.json` (dir 0700, file 0600) with one entry;
   `list-keys` prints it. Re-running the identical `add-key` writes nothing
   (asserted via mtime or a write counter) and exits 0. `add-key` for the
   same role with a *different* fingerprint is refused (error names role +
   both fingerprints) unless `--force`, which overwrites. A malformed
   fingerprint (`SHA256:` wrong length, missing prefix, illegal char) and a
   malformed role (path separator, space, empty) are rejected before any
   write, leaving the registry untouched. `list-keys` on a missing registry
   prints the empty note and exits 0. Marshaled bytes are stable
   (sorted keys) across repeated saves.
10. **Fingerprint resolution + precedence.** With a fake `KeyLocator` keyed
    by fingerprint: an identity whose `key` is a path resolves to that path
    verbatim (no registry read, no locator call) — the existing golden
    fragment is unchanged. An identity whose `key` is `SHA256:…` resolves
    straight through the locator. An identity with no inline key resolves
    role → registry fingerprint → locator → key source, and the agent then
    holds exactly that one key. A role absent from the registry fails
    Prepare with an error naming the role; a fingerprint the locator cannot
    match fails Prepare naming the role and the fingerprint — in both cases
    before any agent is spawned (asserted: no socket dir created, no
    container started).
11. **Locator search order.** Against a fake host exposing a key at more
    than one source, the locator returns the running-agent match before the
    `~/.ssh/*.pub` match before the YubiKey resident match; `~/.ssh/*.pub`
    is walked in sorted order so the chosen match is deterministic. The
    locator reads only public material — the test asserts no private key
    file is opened by faber (only `.pub` reads and the path handed to
    `AddKey`).

## Edge cases

- Empty BindingSet (no remote, no services, no identity, no runtime):
  empty fragment, nil-safe teardown, step still runnable.
- NO_PROXY list order preserved exactly as declared in YAML.
- Retry: a second attempt after a failed run gets a different socket path
  and a fresh resolver invocation (asserted via resolver call count).
- Cancellation during teardown: the detached teardown context still kills
  the agent within its deadline.
