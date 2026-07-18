# Implementation: RoleRegistry store, operations, and resolution

Covers RoleRegistry — the `roles.json` store, the `add-key`/`list-keys`
operations, the `KeyLocator`, and the identity-resolution seam the
IdentityBinding consumes. The two CLI subcommands that expose the operations
are thin dispatch in the config module (`arch_cli.md` / `impl_cli.md`); this
section owns the behavior they compose.

## Registry file and location (internal/security/registry.go)

```go
// Entry is one role's record: a fingerprint plus an optional human label.
// No key material is ever stored.
type Entry struct {
    Fingerprint string `json:"fingerprint"`
    Comment     string `json:"comment,omitempty"`
}

// Registry is the whole role→fingerprint table, marshaled as a JSON object
// keyed by role name.
type Registry map[string]Entry
```

- **Path.** `RegistryPath()` returns `$XDG_CONFIG_HOME/faber/roles.json`
  when `XDG_CONFIG_HOME` is set and absolute, else
  `~/.config/faber/roles.json`. This is faber-engine state, deliberately
  under faber's config home, not beside the user's keys.
- **Load.** `os.ReadFile` + `json.Unmarshal`. A missing file yields an empty
  `Registry`, not an error (list-keys on a fresh install prints nothing;
  add-key creates the file). A present-but-malformed file is a hard error —
  faber does not silently discard a corrupt registry.
- **Save (atomic).** Create the parent dir with `os.MkdirAll(dir, 0o700)`,
  write the marshaled JSON to a temp file **in the same directory**, `chmod`
  it 0600, then `os.Rename` over the target. Same-dir temp + rename makes
  the swap atomic on one filesystem and never leaves a half-written
  registry. JSON is marshaled with sorted keys (Go's `encoding/json` sorts
  map keys) and a trailing newline, so repeated writes are byte-stable.

## Validation (internal/security/registry.go)

Two validators, both applied before any write:

```go
// fingerprint: "SHA256:" + 43 base64 chars (unpadded SHA-256, ssh-keygen form)
var fingerprintRE = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// role: a bare identifier — letters, digits, dot, dash, underscore.
// Rejects path separators, whitespace, and empties so a role name is safe
// as a JSON key and a template reference and can never be a path.
var roleRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
```

A malformed fingerprint or role is a validation error carrying the offending
value; the CLI maps it to the usage exit code (2). These run before the file
is touched, so a bad flag never mutates the registry.

The optional `--comment` is a free-form label, but it is printed back
verbatim through a tabwriter on `list-keys`. It is therefore validated too:
any Unicode control character (newline, tab, terminal escape) is rejected so
an embedded newline cannot forge or misalign a `list-keys` row and an ANSI
escape cannot reach the operator's terminal unfiltered. A comment must be a
single printable line.

## add-key

```
AddKey(reg Registry, role, fingerprint, comment string, force bool) (Registry, changed bool, err error)
```

1. validate `role`, `fingerprint`, and `comment` (printable, single line);
   on failure return the validation error.
2. if `reg[role]` exists:
   - same fingerprint → update the comment if given; `changed` reflects
     whether anything actually differs (identical input ⇒ no-op, no write).
   - different fingerprint and `!force` → refuse with an error naming the
     role, the stored fingerprint, and the new one, telling the user to pass
     `--force` to re-point the role.
   - different fingerprint and `force` → overwrite.
3. else insert the new entry.
4. return the mutated registry; the caller `Save`s it only when `changed`.

Idempotency is by content: re-running the exact same `add-key` writes
nothing and exits 0. This keeps the command safe to script and safe to
re-run in an init flow.

## list-keys

Load the registry and print one line per role in sorted order:
`<role>  <fingerprint>  <comment>` (comment column omitted when empty),
aligned for reading. Output goes to stdout; an empty registry prints a
one-line "no roles registered" note to stderr and exits 0. Fingerprints are
public material and safe to print; no key material exists to leak.

## KeyLocator and resolution (internal/security/resolve.go)

```go
// KeyLocator finds a host key whose fingerprint matches and returns the
// opaque keySource the IdentityBinding's AddKey loads. It reads only public
// material (agent listings, *.pub files, token metadata) — never a private
// key into faber's memory.
type KeyLocator interface {
    Locate(ctx context.Context, fingerprint string) (keySource string, err error)
}

// ResolveIdentity turns a step's declared identity into the keySource the
// ephemeral agent will load, applying explicit-path precedence. The second
// return value is the fingerprint the resolution pinned (the value the loaded
// key MUST carry); it is "" only for the explicit-path branch, where none is
// known.
func ResolveIdentity(ctx context.Context, reg Registry, loc KeyLocator, role string, def config.IdentityDef) (keySource, fingerprint string, err error)
```

`ResolveIdentity` precedence, in order:

1. `def.Key` is a path (any value not matching `fingerprintRE`) → return it
   verbatim with an empty pinned fingerprint. No registry lookup, no locator
   call — today's behavior exactly, so every path-form config is
   byte-identical.
2. `def.Key` is a `SHA256:…` fingerprint → `loc.Locate(fingerprint)`,
   skipping the role→fingerprint hop; the pinned fingerprint is `def.Key`.
3. `def.Key` empty → `reg[role]`; absent ⇒ error
   `identity %q: role not in registry`. Then `loc.Locate(entry.Fingerprint)`;
   the pinned fingerprint is `entry.Fingerprint`.

Any `Locate` miss returns `identity %q: no local key matches fingerprint %s`
— naming both, so the user knows which key to plug in or `add-key`.

**Post-load verification (fail closed).** The locator resolves a `*.pub` to
its private counterpart on a naming convention (`X.pub` → `X`); it never
confirms `X`'s own fingerprint. Because the fingerprint is the entire
cross-system join — the registry pins a role to it and the gate authorizes
signatures by it — the binding verifies the key it *actually* loaded. After
`AddKey` and the key-count check, when the pinned fingerprint is non-empty the
binding asserts the agent holds it; a stale, copied, or planted pub/private
pair whose private half carries a different fingerprint fails Prepare closed,
naming the role, the expected fingerprint, and what the agent holds — at
prepare time, not late at the gate after a wasted box run. The explicit-path
branch pins nothing (no fingerprint is known) and skips this check, preserving
byte-identical behavior for path-form configs.

The default `KeyLocator` (`NewKeyLocator`) searches, first match wins:

1. **running agent** — parse `ssh-add -l`; a line whose fingerprint matches
   yields its comment as the key source when that comment is a readable file
   path, else this source is skipped.
2. **`~/.ssh/*.pub`** — for each, `ssh-keygen -lf <pub>` to read its
   fingerprint; on match the key source is the path with `.pub` trimmed
   (the private counterpart), required to exist and be readable.
3. **YubiKey resident keys** — enumerate resident keys from an attached
   token; on match the key source is the resident-key handle. A resident
   credential is loaded with `ssh-add -K` (download all resident keys from
   the token), not `ssh-add <path>`. The current `AgentController.AddKey`
   issues `ssh-add <keySource>`, treating the source as a positional file
   path, so it **cannot** load a resident handle: this source is not
   reachable by the built-in binding today, and the default resident
   enumerator returns none. Wiring residents requires teaching `AddKey` the
   `ssh-add -K` invocation as well; until then this branch is latent and must
   not be mistaken for a working path.

The locator is behind an interface so unit tests substitute a fake keyed by
fingerprint; the real one shells the ssh binaries through the same exec
adapter the AgentController uses. Determinism: `~/.ssh/*.pub` is walked in
sorted order so a match is stable, and resolution reads no maps in iteration
order.

## Wiring into the binding (internal/security/binding.go, identity.go)

`StepSpec` gains one field so the binding knows *which* role to resolve:

```go
type StepSpec struct {
    // …existing fields…
    Identity     *config.IdentityDef // nil when the template declares none
    IdentityRole string              // the resolved template's identity name; "" when none
}
```

`IdentityBinding` gains a `resolver` (registry + locator) dependency.
`Prepare` replaces its direct read of `step.Identity.Key` with a
`ResolveIdentity(ctx, reg, loc, step.IdentityRole, *step.Identity)` call; the
returned keySource is what it hands to `AddKey`, unchanged from there on. An
empty `def.Key` is no longer an immediate error — it now triggers registry
resolution — but a role that resolves to nothing still fails Prepare, so the
"zero keys ⇒ hard error" guarantee is preserved a step earlier and with a
clearer message. The pipeline that builds `StepSpec` (boxrun/reentry) sets
`IdentityRole` from the template's resolved identity name, alongside the
existing `Identity` lookup.

Nothing about the ephemeral agent, the socket mount, the teardown, or the
"private key never enters the box" invariant changes: resolution only
decides *what path or handle* `AddKey` loads, and it runs host-side before
the agent is even asked to hold a key.
