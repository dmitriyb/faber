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
- Throwaway file keys per identity; a scratch tmpfs.

## Scenarios

1. **Golden fragment.** Assembling the implement step yields the exact
   ordered fragment — network flags, proxy env, NO_PROXY list, remote URL
   env with the repo param spliced (`.../sandbox.git`), pinned host-key
   env, socket mount + SSH_AUTH_SOCK, service handle env, no runtime flag —
   byte-identical across repeated assembly. Adding `runtime: runsc`
   appends exactly `--runtime=runsc` at the end and changes nothing else.
2. **One key per box.** After Prepare, the step's agent socket answers
   with exactly one fingerprint — the implement step's identity, not any
   other configured identity. Two steps prepared concurrently under
   different identities get distinct sockets, each seeing only its own
   key. A resolver that loads two keys produces the >1-key warning with
   both fingerprints; zero keys fails the step.
3. **Teardown always runs.** For each of: container exit 0, container
   exit non-zero, context cancelled mid-run, and a later binding's
   Prepare failing (failing resolver after the agent was spawned) — the
   agent process is gone, the socket directory is removed, and every
   file-mode secret is shredded (content zeroed before removal, asserted
   via an instrumented shred hook). The Prepare-failure case additionally
   asserts no container was ever started.
4. **Host-key modes.** Pinned: the fragment carries the key file's single
   line and nothing tofu-related; an unreadable or empty key file fails
   Prepare naming the remote binding. Tofu: the flag env appears and no
   key line does. Repo-less step: the remote binding contributes nothing
   at all. (Both-modes-configured is a loader rejection, covered in
   config's test catalog; asserted here only as "assembly never sees it".)
5. **File-mode secret containment.** With `agent-api` in file mode: the
   resolver ran host-side exactly once, the fragment contains the ro
   mount `.../agent-api:/run/secrets/agent-api:ro` and **no** env var
   carrying the token; the mount source is 0600 inside the tmpfs scratch;
   a non-tmpfs scratch dir refuses assembly. Switching the service to
   proxy mode invokes no resolver and emits only the base-URL env.
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

## Edge cases

- Empty BindingSet (no remote, no services, no identity, no runtime):
  empty fragment, nil-safe teardown, step still runnable.
- NO_PROXY list order preserved exactly as declared in YAML.
- Retry: a second attempt after a failed run gets a different socket path
  and a fresh resolver invocation (asserted via resolver call count).
- Cancellation during teardown: the detached teardown context still kills
  the agent within its deadline.
