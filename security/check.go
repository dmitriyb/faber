package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/infra"
)

// ContainerCheckRemote is where a check run mounts its remote volume: the
// box's one remote is a bare repository under it, so the clone, the pushes
// and the side-effect verification all happen against a repository nothing
// else can see. The agent module restates the path for its seed verb.
const ContainerCheckRemote = "/faber/remote"

// Seeder is the slice of infra.ContainerRunner the check remote seeds its
// volume through: one short container of the step's image per repository.
type Seeder interface {
	Run(ctx context.Context, spec infra.ContainerSpec) (infra.ContainerResult, error)
}

// NewCheckBindingSet composes the sandbox of a check run (faber check) in
// the fixed order, with three members substituted and the fourth absent:
// no network at all (--network none, whatever the config's section says),
// a bare repository in the named volume as the only remote, the real
// identity binding with every role resolved to keyPath — a key generated
// for the check — and no credential broker. The runtime knob stays. A hook
// that reaches for the gate, the forge or an API fails at that call, which
// is the check's answer.
func NewCheckBindingSet(identity *IdentityBinding, volume, sourceRepo, keyPath string, seeder Seeder, runner infra.CommandRunner, logger *slog.Logger) *BindingSet {
	return &BindingSet{
		bindings: []Binding{
			noNetworkBinding{},
			&checkRemoteBinding{volume: volume, source: sourceRepo, seeder: seeder, runner: runner, seeded: map[string]bool{}, logger: childLogger(logger, "check-remote")},
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
// repo-bound step it serves a bare repository in the check's named volume,
// seeded on the repository's first use from a git bundle of the working
// directory's repository (source) through `faber-box seed-remote` in a
// short container of the step's image — nothing of the host is mounted. A
// local-path remote needs no host-key policy, and the box applies none to
// one.
type checkRemoteBinding struct {
	volume string // the check's named volume, mounted at ContainerCheckRemote
	source string // the working directory's repository; "" when there is none
	seeder Seeder
	runner infra.CommandRunner
	logger *slog.Logger

	mu     sync.Mutex // one seeding per repository, however parallel the steps
	seeded map[string]bool
}

func (b *checkRemoteBinding) Name() string { return "remote" }

func (b *checkRemoteBinding) Prepare(ctx context.Context, step StepSpec) (Contribution, error) {
	if step.Repo == "" {
		return Contribution{}, nil
	}
	if err := checkRepoShape(step.Repo); err != nil {
		return Contribution{}, fmt.Errorf("repo input %q: %w", step.Repo, err)
	}
	if err := b.ensure(ctx, step); err != nil {
		return Contribution{}, err
	}
	return Contribution{Args: []string{
		"-v", b.volume + ":" + ContainerCheckRemote,
		"-e", EnvRemoteURL + "=" + ContainerCheckRemote + "/" + step.Repo + ".git",
	}}, nil
}

// ensure seeds the repository once per check: a bundle of the working
// directory's repository (its committed history only, which is what a hook
// would see on the gateway) goes to `faber-box seed-remote` in a container
// of the step's image with the volume mounted; the verb clones it bare and
// leaves a repository already present alone.
func (b *checkRemoteBinding) ensure(ctx context.Context, step StepSpec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.seeded[step.Repo] {
		return nil
	}
	if b.source == "" {
		return errors.New("check: a repo-bound step needs a git repository in the working directory to stand in for the gateway; run the check from one")
	}
	if step.Image == "" {
		return errors.New("check remote: the step names no image to seed the remote from")
	}
	// The bundle goes through a file, not the command runner's stdout: that
	// stdout is bounded for data-source commands, and a repository's history
	// is not.
	bundleFile, err := os.CreateTemp("", "faber-check-bundle-*")
	if err != nil {
		return fmt.Errorf("check remote: %w", err)
	}
	bundleFile.Close()
	defer os.Remove(bundleFile.Name())
	res, err := b.runner.Run(ctx, infra.CmdSpec{Path: "git", Args: []string{"-C", b.source, "bundle", "create", "--quiet", bundleFile.Name(), "--all"}})
	if err != nil {
		return fmt.Errorf("check remote: bundle the working directory's repository: %w", err)
	}
	if res.ExitCode != 0 {
		// A repository with no commit cannot be bundled: the check needs one.
		return fmt.Errorf("check remote: git bundle exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	bundle, err := os.ReadFile(bundleFile.Name())
	if err != nil {
		return fmt.Errorf("check remote: %w", err)
	}
	// The seed container is a check container too: no network, the step's
	// runtime. The run ids are the box contract's FABER_RUN_UID/GID (the
	// agent module owns the names; security cannot import it).
	bindings := []string{"--network", "none"}
	if step.Runtime != "" {
		bindings = append(bindings, "--runtime="+step.Runtime)
	}
	run, err := b.seeder.Run(ctx, infra.ContainerSpec{
		Name:     fmt.Sprintf("faber-check-seed-%s-%d", strings.ReplaceAll(step.Repo, "/", "-"), time.Now().UnixNano()),
		Image:    step.Image,
		Mounts:   []infra.Mount{{Kind: infra.KindNamed, Name: b.volume, Container: ContainerCheckRemote}},
		Env:      map[string]string{"FABER_RUN_UID": strconv.Itoa(os.Getuid()), "FABER_RUN_GID": strconv.Itoa(os.Getgid())},
		Bindings: bindings,
		// The runner's stdin stream; no secret rides it here.
		StdinSecrets: bundle,
		Entry:        []string{ContainerEntry, "seed-remote", step.Repo},
	})
	if err != nil {
		return fmt.Errorf("check remote: seed %s: %w", step.Repo, err)
	}
	if run.ExitCode != 0 {
		return fmt.Errorf("check remote: seed %s exited %d: %s", step.Repo, run.ExitCode, strings.TrimSpace(string(run.Output)))
	}
	b.seeded[step.Repo] = true
	b.logger.DebugContext(ctx, "check remote seeded", "repo", step.Repo, "volume", b.volume)
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
