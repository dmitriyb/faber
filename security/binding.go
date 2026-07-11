package security

import (
	"context"

	"github.com/dmitriyb/faber/config"
)

// Binding turns one slice of resolved step config into run-argv material.
// Bindings whose configuration is absent from the StepSpec contribute nothing
// and appear nowhere in the fragment.
type Binding interface {
	// Name identifies the binding in errors and logs:
	// "network" | "remote" | "identity" | "credentials" | "runtime".
	Name() string

	// Prepare performs host-side setup (spawn, read, resolve) and returns the
	// contribution. On error, Prepare has already undone its own partial
	// work; the BindingSet unwinds only previously prepared bindings. A
	// returned Teardown must be safe to call exactly once; nil means nothing
	// to undo.
	Prepare(ctx context.Context, step StepSpec) (Contribution, error)
}

// Contribution is one binding's share of the docker-run argv: complete,
// internally ordered tokens consumed verbatim (env vars ride inside Args as
// "-e" pairs, mounts as "-v" pairs — one flat fragment, no merge step).
type Contribution struct {
	Args     []string
	Teardown func(ctx context.Context) error
}

// StepSpec is the resolved slice of configuration one step attempt needs.
// Bindings never read YAML or the IR; the caller (the pipeline executor, via
// the agent module) resolves everything first. ScratchDir must be a private
// per-attempt directory (0700, tmpfs-backed when file-mode credentials are
// declared): fresh assembly per attempt is what makes between-attempt cleanup
// sound, and it is also what gives retries a new socket path and a fresh
// resolver invocation.
type StepSpec struct {
	NodeID     string
	Network    *config.NetworkDef
	Remote     *config.RemoteDef
	Identity   *config.IdentityDef // nil when the template declares none
	Services   map[string]config.ServiceDef
	Runtime    string // "" = platform default runtime
	Repo       string // resolved repo input; "" = repo-less step
	ScratchDir string // per-attempt private dir, 0700
}
