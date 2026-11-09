# Implementation: Binding argv contributions

Covers NetworkBinding, RemoteBinding, IdentityBinding, and BindingSet.
(CredentialBroker's handle shapes are in "Resolver and handle shapes".)

## Core types (internal/security/binding.go)

```go
// Binding turns one slice of resolved step config into run-argv material.
type Binding interface {
    Name() string // "network" | "remote" | "identity" | "credentials" | "runtime"

    // Prepare performs host-side setup (spawn, read, resolve) and returns
    // the contribution. A returned Teardown must be safe to call exactly
    // once; nil means nothing to undo.
    Prepare(ctx context.Context, step StepSpec) (Contribution, error)
}

type Contribution struct {
    Args     []string          // docker run flags, internally ordered, consumed verbatim
    Secrets  map[string]Secret // file-mode tokens (name→Secret); nil except the credentials binding
    Attach   func(ctx context.Context, container string) error // served beside the running container (the identity relay); nil = none
    Teardown func(ctx context.Context) error
}

type StepSpec struct { // the resolved slice of config one step needs
    NodeID       string
    Network      *config.NetworkDef
    Remote       *config.RemoteDef
    Identity     *config.IdentityDef // nil when the template declares none
    IdentityRole string              // resolved template's identity name; "" when none — the registry lookup key
    Services     map[string]config.ServiceDef
    Runtime      string              // "" = platform default
    Repo         string              // resolved repo input; "" = repo-less step
    ScratchDir   string              // per-step private dir, 0700
}
```

Env vars ride inside `Args` as `-e` pairs; mounts as `-v` pairs — one flat,
ordered fragment, no separate merge step to introduce nondeterminism. The
credentials binding's file mode also puts one `--tmpfs <ContainerSecretsDir>`
in `Args` (once) and its resolved tokens in `Secrets`; the tokens themselves
never enter `Args` — they leave the security module only as the encoded
`Assembled.SecretsStdin` payload.

## Per-binding argv shapes

| Binding | Emits |
|---|---|
| network (proxy) | `--network <name>`, `-e HTTPS_PROXY=<proxy>`, `-e HTTP_PROXY=<proxy>`, `-e NO_PROXY=<list>` |
| network (nftables) | `--network <name>`, `--cap-add NET_ADMIN` (no proxy env; root entrypoint loads rules, drops to agent user) |
| remote | `-e FABER_REMOTE_URL=<prefix>/<repo>.git`, then one of `-e FABER_HOST_KEY=<key line>` (pinned) or `-e FABER_HOST_KEY_TOFU=1` |
| identity | `--tmpfs /faber/agent`, `-e SSH_AUTH_SOCK=/faber/agent/agent.sock`, optional `-e FABER_SIGNING_KEY=<pub line>`; and the attach: the relay exec the proxy is served over |
| runtime | `--runtime=<value>` |

The `FABER_*` names are the box env contract shared with the agent module's
entry program; they are defined once in a shared constants file, not
restated per binding.

- **network.Prepare** verifies the named network exists via the
  DockerClient (structured inspect) — the only docker call any binding
  makes — and joins `no_proxy` with commas in declared order. Proxy and
  nftables modes are mutually exclusive; the loader enforced that, so
  Prepare just switches on which is set.
- **remote.Prepare** returns an empty contribution when `step.Repo == ""`
  or `Remote == nil`. Pinned mode reads `host_key_file` at Prepare time
  (os.ReadFile, trimmed single line; empty or unreadable ⇒ error), so a
  bad key fails the step before launch, not inside the box.
- **identity.Prepare** first resolves the key source via
  `ResolveIdentity(ctx, reg, loc, step.IdentityRole, *step.Identity)` —
  explicit path verbatim, else registry+locator (see
  `impl_role_registry.md`); an empty inline key now triggers registry
  resolution rather than an immediate error. It then takes the run's
  **role agent** for that key (`roleAgentFor`, signqueue.go, under the
  binding's mutex): on first use it shells `ssh-agent -a <tmp>/agent.sock`
  through the exec adapter, loads the one resolved key, then lists keys: 0
  ⇒ error, >1 ⇒ `slog.Warn` with fingerprints; when resolution pinned a
  fingerprint (every branch but the explicit path), it asserts the agent
  holds that fingerprint and fails closed otherwise — a mismatched
  pub/private pair is caught here, not at the gate — then dials the socket
  with `agent.NewClient` and keeps the exposed keys (`exposedKeys`: the one
  key, or the pinned one among several). Per attempt it builds a
  `signProxy{keys, upstream, queue}` — `List` → the exposed keys,
  `Sign`/`SignWithFlags` → `queue.Lock` then the upstream client, `Signers`
  → upstream under the queue, `Add`/`Remove`/`RemoveAll`/`Lock`/`Unlock` →
  `errAgentReadOnly`, `Extension` → `agent.ErrExtensionUnsupported` — and
  contributes it as `Attach`: `serveRelay` (signqueue.go) waits for the
  container with `infra.ExecWhenRunning` over the binding's `Exec` seam (the
  docker client, wired by cmd; nil fails the attach naming it), execs
  `/faber/bin/faber-box relay` with an `os.Pipe` as the relay's stdin (an
  `*os.File`, so the exec's wait never blocks on a copier that has nothing
  to copy) and an `io.Pipe` as its stdout, serves `agent.ServeAgent(proxy,
  rw)` over the pair until the exec returns, then closes both. An exec that
  ends with an error or a non-zero code is a failure of the attach only
  while the container is still running (`Running`, asked on a short
  detached context): a relay killed with its container, or never exec'd
  into a box that had already exited, is every run's ordinary end and
  returns nil. The contribution has no teardown and creates no host socket; `Close(ctx)`
  stops every role agent (connection, process, directory) and
  `BindingSet.Close` forwards to any binding that implements it. Resolution
  runs before any spawn, so a role that resolves to nothing fails without
  leaving an agent behind.

## BindingSet (internal/security/set.go)

```go
type BindingSet struct{ bindings []Binding } // fixed order, set at construction

type Assembled struct {
    Args        []string
    SecretsStdin []byte // framed JSON payload for the container's stdin; nil = no file-mode secret
    Attach      func(ctx context.Context, container string) error // every contributed attach, concurrently, errors joined; nil when none
    Teardown    func(ctx context.Context) error // reverse order, always all of them
}

func (s *BindingSet) Prepare(ctx context.Context, step StepSpec) (Assembled, error) {
    var args []string
    var undo []func(context.Context) error
    var attaches []func(context.Context, string) error
    secrets := map[string]Secret{}
    for _, b := range s.bindings {
        c, err := b.Prepare(ctx, step)
        if err != nil {
            runReverse(ctx, undo) // unwind what succeeded; errors logged
            return Assembled{}, fmt.Errorf("binding %s: %w", b.Name(), err)
        }
        args = append(args, c.Args...)
        for name, tok := range c.Secrets { // only the credentials binding sets any
            secrets[name] = tok
        }
        if c.Attach != nil {
            attaches = append(attaches, c.Attach)
        }
        if c.Teardown != nil {
            undo = append(undo, c.Teardown)
        }
    }
    var payload []byte
    if len(secrets) > 0 {
        var err error
        if payload, err = encodeSecretsPayload(secrets); err != nil {
            runReverse(ctx, undo)
            return Assembled{}, fmt.Errorf("binding credentials: %w", err)
        }
    }
    return Assembled{Args: args, SecretsStdin: payload, Attach: joinAttach(attaches), Teardown: reverseJoin(undo)}, nil
}
```

`joinAttach` is nil for no contributors and otherwise runs every attach in
its own goroutine against the same context and container name, waits for
all, and joins their errors.

`runReverse`/`reverseJoin` iterate the undo stack last-to-first, call every
function even when earlier ones fail, and join errors. Teardown uses a
context detached from step cancellation (`context.WithoutCancel` + a short
deadline) so a user abort still kills agents. File mode adds no teardown —
its secrets live only in the container's tmpfs and die with the container —
so nothing host-side is left to shred. `encodeSecretsPayload` (the sole
`Secret.reveal()` caller) is defined in "Resolver and handle shapes".

The constructor wires the fixed order — network, remote, identity,
credentials, runtime — and the runtime "binding" is a trivial inline
Binding emitting `--runtime=` or nothing. ContainerRunner receives
`Assembled.Args` and splices the fragment unchanged between its own
resource/mount flags and the image name; it never parses or reorders it.
`Assembled.SecretsStdin` rides alongside into `ContainerSpec.StdinSecrets`, where
infra streams it on the container's stdin, and `Assembled.Attach` into
`ContainerSpec.Attach`, which infra starts beside the container and cancels
when the container ends.

## Check composition (security/check.go)

```go
const ContainerCheckRemote = "/faber/remote"

// Seeder is the slice of infra.ContainerRunner the check remote seeds through.
type Seeder interface { Run(ctx context.Context, spec infra.ContainerSpec) (infra.ContainerResult, error) }

func NewCheckBindingSet(identity *IdentityBinding, volume, sourceRepo, keyPath string,
    seeder Seeder, runner infra.CommandRunner, logger *slog.Logger) *BindingSet
// bindings: noNetworkBinding{}, &checkRemoteBinding{...}, &checkIdentityBinding{inner, key}, runtimeBinding{}
```

- `noNetworkBinding` (name `network`): `--network none`, always.
- `checkRemoteBinding` (name `remote`): nothing for a repo-less step; else
  `checkRepoShape`, then the repository seeded once per name under a mutex:
  `git -C <sourceRepo> bundle create --quiet <tempfile> --all` through the
  command runner into a temporary file (not the runner's stdout, which is
  bounded for data-source commands; a repository with no commit cannot be
  bundled and fails here; a missing `sourceRepo` — no repository in the
  working directory — fails the step before launch, saying so), its bytes as
  the stdin of a seed container the `Seeder` runs — `ContainerSpec{Name:
  faber-check-seed-<repo with / as ->-<nanos>, Image: step.Image, Mounts:
  [{KindNamed, volume, /faber/remote}], Env: {FABER_RUN_UID, FABER_RUN_GID},
  Bindings: [--network none, --runtime=<step.Runtime> when set],
  StdinSecrets: bundle, Entry: [/faber/bin/faber-box, seed-remote, <repo>]}`
  — a check container like the step's own; a non-zero exit fails the step
  naming its stderr tail; then contributing
  `-v <volume>:/faber/remote` and `-e FABER_REMOTE_URL=/faber/remote/<repo>.git`.
  `StepSpec.Image` (the attempt's tag, filled by the pipeline and re-entry)
  is what the seed container runs; `StdinSecrets` here is only the runner's
  stdin stream, no secret rides it. No host-key material.
- `checkIdentityBinding` (name `identity`): nothing when the template
  declares no identity; else the inner binding's Prepare with
  `Identity = {Key: keyPath}` and an empty role — path form, one agent
  shared by every role. `Close` delegates.
- No credentials binding: `Assembled.SecretsStdin` is always empty.

## Determinism

No maps are iterated anywhere in assembly: `no_proxy` is a YAML list,
services are walked via sorted keys (broker side), and each binding's args
are literal slices. Nothing in the fragment is attempt-unique: the role
agent's host socket is faber's own and never in the argv, and the box's
agent socket is a fixed container path. Golden test: assembling the
reference `implement` step twice yields byte-identical fragments.
