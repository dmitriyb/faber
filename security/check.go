package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/infra"
)

// ContainerCheckRemote is where a check run bind-mounts its throwaway bare
// repositories: the box's one remote is a path under it, so the clone,
// the pushes and the side-effect verification all happen against a
// repository nothing else can see.
const ContainerCheckRemote = "/faber/remote"

// NewCheckBindingSet composes the sandbox of a check run (faber check) in
// the fixed order, with three members substituted and the fourth absent:
// no network at all (--network none, whatever the config's section says),
// a bare repository under remoteDir as the only remote, the real identity
// binding with every role resolved to keyPath — a key generated for the
// check — and no credential broker. The runtime knob stays. A hook that
// reaches for the gate, the forge or an API fails at that call, which is
// the check's answer.
func NewCheckBindingSet(identity *IdentityBinding, remoteDir, sourceRepo, keyPath string, runner infra.CommandRunner, logger *slog.Logger) *BindingSet {
	return &BindingSet{
		bindings: []Binding{
			noNetworkBinding{},
			&checkRemoteBinding{dir: remoteDir, source: sourceRepo, runner: runner, logger: childLogger(logger, "check-remote")},
			&checkIdentityBinding{inner: identity, key: keyPath},
			runtimeBinding{},
		},
		logger: childLogger(logger, "check-binding-set"),
	}
}

// noNetworkBinding takes the network binding's place in a check run: the
// box has no route anywhere, companion services included.
type noNetworkBinding struct{}

func (noNetworkBinding) Name() string { return "network" }

func (noNetworkBinding) Prepare(context.Context, StepSpec) (Contribution, error) {
	return Contribution{Args: []string{"--network", "none"}}, nil
}

// checkRemoteBinding takes the remote binding's place in a check run. For a
// repo-bound step it serves a bare repository cloned on first use from the
// working directory's repository (source), bind mounted read-write so the
// box's pushes land there. A local-path remote needs no host-key policy,
// and the box applies none to one.
type checkRemoteBinding struct {
	dir    string // host directory holding <repo>.git bare repositories
	source string // the working directory's repository; "" when there is none
	runner infra.CommandRunner
	logger *slog.Logger

	mu sync.Mutex // one clone per repository, however parallel the steps
}

func (b *checkRemoteBinding) Name() string { return "remote" }

func (b *checkRemoteBinding) Prepare(ctx context.Context, step StepSpec) (Contribution, error) {
	if step.Repo == "" {
		return Contribution{}, nil
	}
	if err := checkRepoShape(step.Repo); err != nil {
		return Contribution{}, fmt.Errorf("repo input %q: %w", step.Repo, err)
	}
	bare := filepath.Join(b.dir, step.Repo+".git")
	if err := b.ensure(ctx, bare); err != nil {
		return Contribution{}, err
	}
	return Contribution{Args: []string{
		"-v", b.dir + ":" + ContainerCheckRemote,
		"-e", EnvRemoteURL + "=" + ContainerCheckRemote + "/" + step.Repo + ".git",
	}}, nil
}

// ensure clones the bare repository once. The clone carries the working
// directory's committed history only, which is what a hook would see on
// the gateway.
func (b *checkRemoteBinding) ensure(ctx context.Context, bare string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := os.Stat(bare); err == nil {
		return nil
	}
	if b.source == "" {
		return errors.New("check: a repo-bound step needs a git repository in the working directory to stand in for the gateway; run the check from one")
	}
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		return fmt.Errorf("check remote: %w", err)
	}
	res, err := b.runner.Run(ctx, infra.CmdSpec{Path: "git", Args: []string{"clone", "--bare", "--quiet", b.source, bare}})
	if err != nil {
		return fmt.Errorf("check remote: clone the working directory's repository: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("check remote: git clone --bare exited %d: %s", res.ExitCode, string(res.Stderr))
	}
	b.logger.DebugContext(ctx, "check remote created", "bare", bare)
	return nil
}

// checkIdentityBinding is the real identity binding with every role
// resolved to the check's generated key: path form, so neither the registry
// nor the locator — nor the operator's own keys — are ever consulted. A
// template that declares no identity still gets none.
type checkIdentityBinding struct {
	inner *IdentityBinding
	key   string
}

func (b *checkIdentityBinding) Name() string { return "identity" }

func (b *checkIdentityBinding) Prepare(ctx context.Context, step StepSpec) (Contribution, error) {
	if step.Identity == nil {
		return Contribution{}, nil
	}
	step.Identity = &config.IdentityDef{Key: b.key}
	step.IdentityRole = ""
	return b.inner.Prepare(ctx, step)
}

func (b *checkIdentityBinding) Close(ctx context.Context) error { return b.inner.Close(ctx) }
