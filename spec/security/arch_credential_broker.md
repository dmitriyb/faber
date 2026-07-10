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
  host-side and mounts it read-only as a tmpfs-backed file at
  `/run/secrets/<service>` — never env, never an image layer, never the
  journal. The file is 0600, exists only for the container's lifetime, and
  is shredded (overwritten, then removed) after the run on every exit path.
  Degraded because the agent user can read it; contained because it dies
  with the step. The box's secrets phase exports each `/run/secrets/*` file
  into the agent's process env by contract.
- **helper**: for tools with a credential-helper protocol, the declaration
  names the helper configuration to inject (endpoint or command the in-box
  tool calls at use time, answered by a broker outside the box). The shape
  is tool-specific config passthrough; faber validates only that the mode's
  required fields are present.

## The resolver

`get_token(service)` is an opaque user command declared once
(`credentials.resolver`) and invoked host-side, never inside a container.
Keychain, secret-service, an encrypted vault CLI, an env lookup, a static
file — faber is agnostic; the contract is argv `[resolver, service]`,
secret on stdout, non-zero exit means the step fails before launch. The
result is typed as an opaque `Secret` whose string formatting is redacted,
so it cannot leak through logs, error wrapping, or debug output. Only the
file mode ever unwraps it, at the moment of writing the mount source.

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
