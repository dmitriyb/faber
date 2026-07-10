# Implementation: Resolver and handle shapes

Covers CredentialBroker.

## The Secret type (internal/security/secret.go)

```go
// Secret is an opaque credential value. It can be written to a file mount
// source; it cannot be printed.
type Secret struct{ v []byte }

func (Secret) String() string               { return "[redacted]" }
func (Secret) Format(f fmt.State, _ rune)   { io.WriteString(f, "[redacted]") }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

func (s Secret) reveal() []byte { /* unexported: only fileHandle calls it */ }
```

Every formatting path — `%s`, `%v`, `%+v`, error wrapping, JSON encoding —
yields `[redacted]`. The raw bytes are reachable only through the
unexported accessor inside this package, and only the file mode uses it.

## Resolver invocation (host-side, never in a container)

```go
type Resolver interface {
    GetToken(ctx context.Context, service string) (Secret, error)
}
```

The production implementation shells the user's `credentials.resolver`
command through infra's CommandRunner: argv `[resolver, service]`, stdin
closed, stdout captured (trailing newline trimmed) into a Secret, stderr
passed through to faber's log. Non-zero exit or empty stdout is an error
naming the service but never any output content. There is no caching:
each step attempt that needs a token invokes the resolver afresh, which is
also the whole first-pass refresh story (see the expiry seam). What the
command does — OS keychain, an encrypted vault CLI, env, a static file —
is invisible by design.

## Handle shapes per mode

The broker is the `credentials` Binding: `Prepare` walks the step's
declared services **via sorted service names** and appends each handle's
flags, so the fragment stays deterministic.

- **proxy** — no resolver call, no secret in faber at all:

  ```
  -e FABER_SERVICE_<NAME>_URL=<endpoint>
  ```

  `<NAME>` is the service name uppercased with `-` → `_`. The endpoint is
  the unauthenticated local URL on the internal network; the user's
  injecting proxy behind it holds the real credential. The box (or its
  hooks) points the tool's base URL at this env var.

- **file** — degraded, requires the service's explicit `mode: file`:

  ```go
  tok, err := r.GetToken(ctx, name)              // host-side
  path := filepath.Join(step.ScratchDir, name)   // tmpfs-backed scratch
  writeFile0600(path, tok.reveal())
  args = append(args, "-v", path+":/run/secrets/"+name+":ro")
  ```

  Never env: env leaks into `docker inspect`, child processes, and crash
  dumps; a file mount is visible only inside the mount namespace. The
  scratch dir must be tmpfs-backed (verified at startup; refused
  otherwise) so the token never touches disk. Teardown shreds:
  overwrite the file's length with zeros, sync, remove — and runs on every
  exit path via the BindingSet's reverse teardown. In-box contract (agent
  module): the secrets phase exports each `/run/secrets/*` file as an
  UPPERCASE env var inside the box process only.

- **helper** — config passthrough for tools with a credential-helper
  protocol: the declaration's helper fields are forwarded as env
  (`FABER_HELPER_<NAME>_*`) for the box hooks to install into the tool's
  config. Faber validates required fields per mode at load time and
  otherwise treats the values as opaque.

## Mode selection and validation

`mode` is one of `proxy | file | helper`; the loader enforces the
per-mode required fields (`proxy`/`helper` need an endpoint, `file` needs
none) and rejects unknown modes. `file` is deliberately noisy: assembly
logs one warning per file-mode service per run, naming it the degraded
path, so drift from proxy mode stays visible.

## Failure behavior

Any resolver failure, unwritable scratch file, or non-tmpfs scratch dir
fails the step before a container exists, with the binding name and
service in the structured error. Secrets never appear in errors — the
Secret type makes that a property of the type system, not of discipline.
