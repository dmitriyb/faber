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
    Teardown func(ctx context.Context) error
}

type StepSpec struct { // the resolved slice of config one step needs
    NodeID     string
    Network    *config.NetworkDef
    Remote     *config.RemoteDef
    Identity   *config.IdentityDef // nil when the template declares none
    Services   map[string]config.ServiceDef
    Runtime    string            // "" = platform default
    Repo       string            // resolved repo input; "" = repo-less step
    ScratchDir string            // per-step private dir, 0700
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
| identity | `-v <sock>:/ssh-agent`, `-e SSH_AUTH_SOCK=/ssh-agent`, optional `-e FABER_SIGNING_KEY=<pub line>`, platform group flag when needed |
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
- **identity.Prepare** shells `ssh-agent -a <ScratchDir>/agent.sock`
  through the exec adapter, loads the one key via the identity's resolver
  seam, then lists keys: 0 ⇒ error, >1 ⇒ `slog.Warn` with fingerprints.
  Teardown kills the agent PID and removes the socket directory,
  independent errors joined with `errors.Join`.

## BindingSet (internal/security/set.go)

```go
type BindingSet struct{ bindings []Binding } // fixed order, set at construction

type Assembled struct {
    Args        []string
    SecretsStdin []byte // framed JSON payload for the container's stdin; nil = no file-mode secret
    Teardown    func(ctx context.Context) error // reverse order, always all of them
}

func (s *BindingSet) Prepare(ctx context.Context, step StepSpec) (Assembled, error) {
    var args []string
    var undo []func(context.Context) error
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
    return Assembled{Args: args, SecretsStdin: payload, Teardown: reverseJoin(undo)}, nil
}
```

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
`Assembled.SecretsStdin` rides alongside into `RunSpec.StdinSecrets`, where
infra streams it on the container's stdin.

## Determinism

No maps are iterated anywhere in assembly: `no_proxy` is a YAML list,
services are walked via sorted keys (broker side), and each binding's args
are literal slices. Golden test: assembling the reference `implement` step
twice yields byte-identical fragments.
