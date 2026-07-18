# RoleRegistry — a global role→fingerprint table and fingerprint resolution

## What it is

The persistent map from a **role name** to a **key fingerprint**, plus the
host-side resolution that turns a template's `identity: <name>` into a live
key source the ephemeral agent can load. It is the faber half of the agreed
identity scheme: role keys live in the user's `~/.ssh` (or a YubiKey) under
whatever filenames the user chose, and the only thing that crosses systems
is the fingerprint (`SHA256:…`). faber never creates keys, never writes key
material, and never learns which server-side role a fingerprint maps to —
that table is the gate's. faber keeps only `role → fingerprint`.

The registry is faber-engine state (not the user's key material), so it
lives under faber's own config home, distinct from the keys it points at.

## The store

A single JSON file, `$XDG_CONFIG_HOME/faber/roles.json` (fallback
`~/.config/faber/roles.json`):

```json
{
  "implementer_work": { "fingerprint": "SHA256:abc…", "comment": "yubikey 5c" },
  "reviewer":         { "fingerprint": "SHA256:def…" }
}
```

Role names are free and may be split per project (`implementer_work`,
`implementer_personal`); the registry keys off the role, never off a
filename, and holds nothing but a fingerprint and an optional human label.
The directory is created lazily (0700) on first write; the file is written
0600. There is exactly one registry per user; it is global, not per-repo
(the per-repo `fingerprint → role` direction is the gate's concern and
lives entirely outside faber).

## Two operations (exposed as CLI subcommands)

- **add-key** `role, fingerprint, comment?` — upsert one entry. The
  fingerprint is validated (`SHA256:` + 43 base64 chars); a malformed
  fingerprint is rejected before any write. The role name is validated as a
  bare identifier (no path separators, no whitespace) since it is both a
  JSON key and a template reference. Idempotent: writing the same
  role+fingerprint again is a no-op success. Re-pointing an existing role
  at a *different* fingerprint is refused unless the caller opts in
  explicitly (`--force`) — a silent overwrite would swap a box's whole
  credential out from under a running project. The optional comment is
  validated as a single printable line (control characters — newlines, tabs,
  terminal escapes — are rejected) since `list-keys` prints it back to the
  operator's terminal verbatim. Only the fingerprint and label are ever
  written; no key material.
- **list-keys** — read the registry and report every entry (role,
  fingerprint, comment) in a stable, sorted-by-role order. A missing
  registry file reads as empty, not an error.

Both are thin: the CLI (config module) parses flags and dispatches; the
registry owns the load/validate/atomic-write logic. See
`impl_role_registry.md`.

## Fingerprint resolution — the real integration

This is where the registry earns its keep. When a step's identity carries
**no explicit key path**, the binding resolves the role to a live key in
two hops, entirely host-side:

1. **role → fingerprint.** Look the role up in the registry. A role absent
   from the registry is a resolution error naming the role.
2. **fingerprint → key source.** A `KeyLocator` searches the host for a key
   whose fingerprint matches, in a fixed order:
   1. the **running ssh-agent** (`ssh-add -l`) — the fast path, confirming
      the user already holds the key and yielding its source path from the
      listing's comment when that names a readable file;
   2. **`~/.ssh/*.pub`** — each public key's fingerprint is computed
      (`ssh-keygen -lf`); on a match, the private counterpart (the same
      path without `.pub`) is the key source;
   3. **YubiKey resident keys** — enumerated from an attached token; on a
      match, the resident-key handle is the key source. A resident handle is
      loaded with `ssh-add -K`, which the current `AddKey` (`ssh-add <path>`)
      does not issue, so this source is latent — unreachable by the built-in
      binding, and the default enumerator returns none — until `AddKey` learns
      that invocation.

   The first match wins; no match is a resolution error naming the role
   **and** the fingerprint it was looking for. The locator produces the
   same opaque `keySource` string the IdentityBinding's `AddKey` already
   consumes — a file path for a file key, a token handle for a hardware
   key — so the ephemeral-agent lifecycle downstream is unchanged.

Resolution reads only public material (fingerprints, `.pub` files, agent
listings). It never reads a private key into faber's memory: for a file key
it hands `ssh-add` the path; for a hardware key it hands over a token
handle. The private key still never enters the box (the unchanged
invariant) — the box only ever sees the forwarded agent socket.

The `~/.ssh/*.pub → private counterpart` hop is a naming convention, not a
cryptographic check, so after the located key is actually loaded the binding
verifies the agent holds the fingerprint the registry pinned and fails closed
otherwise (see `arch_identity_binding.md`). A stale, copied, or planted
pub/private pair is caught at prepare time, not late at the gate.

## Precedence — explicit path wins

An explicit `identities.<name>.key` in the config takes precedence over the
registry, so existing configs are unaffected:

| `identities.<name>.key` | Resolution |
|---|---|
| a path (e.g. `./keys/implementer`) | used verbatim as today — no registry lookup, byte-identical behavior |
| a `SHA256:…` fingerprint | resolved through the `KeyLocator` directly (skip the role→fingerprint hop) |
| empty / absent | role name → registry → fingerprint → `KeyLocator` |

Every current config sets a path, so the smoke run and every `examples/`
config keep resolving exactly as before. The registry path is reached only
when a template declares an identity with no inline key.

## Failure is loud and early

A resolution failure — role not in the registry, or no local key matches
the fingerprint — is a clear error at prepare time (the earliest point the
run-time binding flow executes), naming the role and, where known, the
fingerprint. It fails the step before any container launches, exactly like
an unreadable host key does today. faber never falls back to "some other
key": a role that cannot be resolved to its one intended key is a hard
stop, because a wrong key is a wrong identity.

Requirements implemented: Role-fingerprint identity registry.
