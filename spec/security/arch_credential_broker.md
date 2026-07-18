# CredentialBroker — secrets as handles, never contents

## What it is

The uniform rule for every credential that is not an SSH key: the box never
holds the secret; it holds a *handle* to an out-of-container broker. The
broker component turns each `credentials.services` declaration into a
per-step contribution whose shape depends on the declared mode — because no
universal "token-agent" exists, the handle shape is per-tool.

## Handle modes

- **proxy** (preferred): the service declaration carries an endpoint URL on
  the internal network; the binding exports it as the service's base-URL
  env. The box talks to an *unauthenticated* local endpoint, and the user's
  auth-injecting proxy — a companion service, opaque to faber — adds the
  real credential on the way out. Faber never touches the secret at all in
  this mode; its entire contribution is one env var.
- **file** (degraded, explicit opt-in): faber resolves the raw token
  host-side (it stays in host RAM) and delivers it into a **container
  tmpfs** — never a host file, never env, never argv, never an image layer,
  never the journal. The binding emits `--tmpfs /run/secrets` once (RAM in
  the container) and carries the resolved `name→token` secrets as a payload
  on its Contribution; the runner streams that payload over the container's
  stdin (`docker run -i`), and faber-box writes each `0600` file at
  `/run/secrets/<service>` before exporting it to env. The file exists only
  for the container's lifetime and dies with it — no host tmpfs, no shred.
  Degraded because the agent user can read it; contained because it never
  leaves the container. The box's secrets phase exports each
  `/run/secrets/*` file into the agent's process env by contract, unchanged.
  Path: host-RAM → docker-API stdin stream → container-RAM.
- **helper**: for tools with a credential-helper protocol, the declaration
  names the helper configuration to inject (endpoint or command the in-box
  tool calls at use time, answered by a broker outside the box). The shape
  is tool-specific config passthrough; faber validates only that the mode's
  required fields are present.

## File mode: security posture

Delivering the token over the container's stdin, not a host bind mount, is
what makes file mode work on every platform (macOS has no host tmpfs, so
the old verified-host-tmpfs mount was unusable there) and tightens the
exposure at the same time. The stated properties, without overclaiming:

- **The raw token never becomes a host file** — there is no host file at all,
  not even on a host tmpfs. This also improves Linux, which previously wrote
  the token to a host tmpfs file. (Honestly stated: the token still transits
  host process RAM, and a container `--tmpfs` page is host-kernel RAM that can
  be paged to host swap under memory pressure — the same residual exposure the
  old host-tmpfs file already had; `mlock`-ing those pages is out of scope for
  the first pass.)
- **Not in argv, `-e`, `docker inspect`, or `docker logs`.** It rides the
  stdin stream, which none of those surfaces capture.
- **The docker-API stdin transit is not a new exposure.** Reaching the
  docker socket is already root-equivalent on the host; a caller who could
  read the stdin stream could already do anything.
- **The resulting posture equals the old file mode's end state**: the token
  lands in the agent's container env plus a `0600` tmpfs file at
  `/run/secrets/<service>`. That is still weaker than proxy mode — the agent
  holds the token — so file mode stays the explicit, noisy opt-in. Only the
  token's *origin* changed (container stdin, not a host bind mount); the
  in-container contract is identical. The `/run/secrets` mount is now a
  run-user-writable container tmpfs (the box's gated chown makes it writable so
  phase 3 can create the files) rather than the old read-only (`:ro`) host
  bind, but token exposure is unchanged: the agent already holds the plaintext,
  and the phase-3 export finishes long before the phase-9 agent runs, so there
  is no TOCTOU between writing the file and using it.

Interactive re-entry is composed without the credential broker at all: the
debug shell observes a failed step, it never runs the agent, and — because the
raw shell replaces the box's phase sequencer — it cannot materialize the stdin
secrets payload anyway. So the re-entry shell carries no credentials: secrets
are neither resolved (no resolver call) nor streamed, and an operator who needs
one sets it inside the shell by hand.

## The resolver

`get_token(service)` is an opaque user command declared once
(`credentials.resolver`) and invoked host-side, never inside a container.
Keychain, secret-service, an encrypted vault CLI, an env lookup, a static
file — faber is agnostic; the contract is argv `[resolver, service]`,
secret on stdout, non-zero exit means the step fails before launch. The
result is typed as an opaque `Secret` whose string formatting is redacted,
so it cannot leak through logs, error wrapping, or debug output. Only the
file mode ever unwraps it, at the single moment of encoding the stdin
secrets payload (base64 inside one JSON object).

The upstream forge credential is the canonical non-example: it is not in
any service declaration, no box ever receives it in any mode, and faber
never resolves it — it lives solely inside the user's gate service.

## Deferred seam: secret expiry mid-run

A delegated credential can lapse mid-step: an expiring token behind the
injecting proxy, a hardware-backed agent socket invalidated by device
removal. Backlog: detection, refresh policy, and whether an interrupted
step restarts or resumes. The first pass reserves the seam and treats
expiry as an ordinary step failure — the agent's calls start failing, the
step fails with that error in its result, and standard retry (fresh
resolver invocation on the next attempt) is the only refresh mechanism.

Requirements implemented: Credential broker handles; Deferred: secret
expiry mid-run.
