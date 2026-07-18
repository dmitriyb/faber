# Implementation: Resolver and handle shapes

Covers CredentialBroker.

## The Secret type (internal/security/secret.go)

```go
// Secret is an opaque credential value. It can be encoded into the stdin
// secrets payload; it cannot be printed.
type Secret struct{ v []byte }

func (Secret) String() string               { return "[redacted]" }
func (Secret) Format(f fmt.State, _ rune)   { io.WriteString(f, "[redacted]") }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

func (s Secret) reveal() []byte { /* unexported: only encodeSecretsPayload calls it */ }
```

Every formatting path — `%s`, `%v`, `%+v`, error wrapping, JSON encoding —
yields `[redacted]`. The raw bytes are reachable only through the
unexported accessor inside this package, and only the stdin-payload encoder
(`encodeSecretsPayload`, in set.go) uses it.

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
flags, so the fragment stays deterministic. File-mode services also
accumulate their resolved tokens into the Contribution's `Secrets` map
(`name → Secret`); proxy and helper modes leave it nil.

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
  tok, err := r.GetToken(ctx, name)          // host-side, stays in RAM
  if err != nil { return Contribution{}, err }
  if c.Secrets == nil {
      c.Secrets = map[string]Secret{}
      // one tmpfs for the whole secrets dir, emitted once regardless of
      // how many file-mode services — RAM in the container, no host file.
      c.Args = append(c.Args, "--tmpfs", contract.ContainerSecretsDir)
  }
  c.Secrets[name] = tok                      // delivered over stdin, see below
  ```

  Never env, never argv, never a host file: env leaks into `docker
  inspect`, child processes, and crash dumps; a host file (even on a host
  tmpfs) is an exposure the container never needs. Instead the token
  rides the Contribution's `Secrets` payload and is streamed into the
  container's own tmpfs over stdin (see "Stdin secrets payload" below). The
  `--tmpfs <ContainerSecretsDir>` flag (`/run/secrets`) makes that dir
  writable RAM in the container; it is emitted **exactly once** even with
  several file-mode services. There is no host file to shred, so file mode
  contributes no teardown. In-box contract (agent module): faber-box
  materializes each payload entry as a `0600` file under `/run/secrets/`
  and the secrets phase then exports each `/run/secrets/*` file as an
  UPPERCASE env var inside the box process only.

- **helper** — config passthrough for tools with a credential-helper
  protocol: the declaration's helper fields are forwarded as env
  (`FABER_HELPER_<NAME>_*`) for the box hooks to install into the tool's
  config. Faber validates required fields per mode at load time and
  otherwise treats the values as opaque.

## Stdin secrets payload

The resolved file-mode tokens never become an argv flag or a host file;
they travel as a single JSON object written to the container's stdin. The
merge and encode happen once, in `BindingSet.Prepare` (impl_bindings.md):
it unions every Contribution's `Secrets` map (only the credentials binding
sets one) and, if non-empty, encodes the framed payload into
`Assembled.SecretsStdin []byte`:

```go
// encodeSecretsPayload is the sole reveal() caller. Sorted keys keep the
// bytes deterministic; each token is base64 (std encoding) so arbitrary
// bytes survive JSON transit intact.
func encodeSecretsPayload(secrets map[string]Secret) ([]byte, error) {
    m := make(map[string]string, len(secrets))
    for _, name := range slices.Sorted(maps.Keys(secrets)) {
        m[name] = base64.StdEncoding.EncodeToString(secrets[name].reveal())
    }
    return json.Marshal(m) // {"<name>":"<base64(token)>", ...}
}
```

The caller that bridges security to infra does so as one atomic step: it copies
`Assembled.SecretsStdin` into `RunSpec.StdinSecrets` and, in the same move, sets
the non-secret signal `FABER_SECRETS_STDIN=1` (`contract.EnvSecretsStdin`) in
`RunSpec.Env` so the box knows a payload is coming. These two MUST be written
together — a non-empty `StdinSecrets` with the flag unset makes the box skip its
stdin read (phase 3 gates on the flag) and silently lose the credential, so the
payload and its signal are never set one without the other. Infra attaches stdin (`docker run -i`),
writes those bytes, and closes it (see infra's run argv). faber-box reads
stdin to EOF, JSON-decodes, base64-decodes each value, and writes
`<ContainerSecretsDir>/<name>` at `0600` when `FABER_SECRETS_STDIN=1`
(agent module). Path: host-RAM → docker-API stdin stream → container-RAM.
This is the one place a `Secret` is unwrapped, mirroring the trust boundary
the old host-file write sat on — base64 of the token leaves the type here
and nowhere else.

## Mode selection and validation

`mode` is one of `proxy | file | helper`; the loader enforces the
per-mode required fields (`proxy`/`helper` need an endpoint, `file` needs
none) and rejects unknown modes. `file` is deliberately noisy: assembly
logs one warning per file-mode service per run, naming it the degraded
path, so drift from proxy mode stays visible.

## Failure behavior

Any resolver failure fails the step before a container exists, with the
binding name and service in the structured error. There is no host file to
write and no scratch dir to verify, so those failure modes are gone by
construction. Secrets never appear in errors — the Secret type makes that a
property of the type system, not of discipline.
