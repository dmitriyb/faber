package security

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

// Credential handle modes (the closed set the loader admits).
const (
	modeProxy  = "proxy"
	modeFile   = "file"
	modeHelper = "helper"
)

// serviceNamePattern mirrors the loader's closed charset for service names.
// Every mode embeds the name somewhere structural — env-var names (proxy,
// helper), the -v mount spec and the /run/secrets path (file) — so a name
// carrying ':', '=', '/', or spaces must fail closed here too, naming the
// service, rather than surface as an opaque docker error mid-run.
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// CredentialBroker is the credential-delegation binding (requirement
// 0c5bc0f678b7): the box never holds a secret, it holds a handle to an
// out-of-container broker, and the handle's shape is per-tool because no
// universal token-agent exists.
//
//   - proxy (preferred): one env var carrying the service's unauthenticated
//     endpoint URL on the internal network; the user's auth-injecting proxy
//     behind it holds the real credential. Faber never touches the secret.
//   - file (degraded, explicit opt-in): the raw token is resolved host-side
//     and mounted read-only as a tmpfs-backed 0600 file under
//     /run/secrets/<service> — never env (env leaks into docker inspect,
//     child processes, and crash dumps), never an image layer, never the
//     journal — and shredded after the run on every exit path.
//   - helper: config passthrough for tools with a credential-helper
//     protocol, forwarded as FABER_HELPER_<NAME>_* env.
//
// Deferred secret-expiry seam (requirement 0157d30de15f), first pass: no
// detection, no refresh — an expired credential is an ordinary step failure,
// and retry's fresh resolver invocation is the only refresh mechanism.
//
// The upstream forge credential is the canonical non-example: it is never a
// service declaration, and faber never resolves it — it lives solely inside
// the user's gate service.
type CredentialBroker struct {
	resolver Resolver
	logger   *slog.Logger

	// isTmpfs, shred, and writeFile are seams for tests; production uses the
	// platform statfs check, the overwrite-sync-remove shredder, and
	// os.WriteFile.
	isTmpfs   func(path string) (bool, error)
	shred     func(path string) error
	writeFile func(path string, data []byte, perm os.FileMode) error
}

// NewCredentialBroker wires the broker to the resolver seam. resolver may be
// nil when no credentials.resolver is configured; only file mode needs it.
func NewCredentialBroker(resolver Resolver, logger *slog.Logger) *CredentialBroker {
	return &CredentialBroker{
		resolver:  resolver,
		logger:    childLogger(logger, "credential-broker"),
		isTmpfs:   isTmpfsDir,
		shred:     shredFile,
		writeFile: os.WriteFile,
	}
}

// Name implements Binding.
func (b *CredentialBroker) Name() string { return "credentials" }

// Prepare walks the step's declared services in sorted name order (the
// fragment stays deterministic) and appends each handle's flags. Any resolver
// failure, unwritable scratch file, or non-tmpfs scratch dir fails the step
// before a container exists; file material already written is shredded before
// returning. Errors name the binding and service, never secret content — the
// Secret type makes that structural.
func (b *CredentialBroker) Prepare(ctx context.Context, step StepSpec) (Contribution, error) {
	if len(step.Services) == 0 {
		return Contribution{}, nil
	}
	var args []string
	var files []string
	fail := func(err error) (Contribution, error) {
		for _, f := range files {
			if serr := b.shred(f); serr != nil {
				b.logger.WarnContext(ctx, "shred after failed credential setup", "err", serr)
			}
		}
		return Contribution{}, err
	}
	tmpfsChecked := false
	for _, name := range slices.Sorted(maps.Keys(step.Services)) {
		if !serviceNamePattern.MatchString(name) {
			// The loader rejects these; fail closed anyway for all modes.
			return fail(fmt.Errorf("service %q: invalid name (must match %s)", name, serviceNamePattern))
		}
		svc := step.Services[name]
		switch svc.Mode {
		case modeProxy:
			if svc.Endpoint == "" {
				return fail(fmt.Errorf("service %q: proxy mode requires an endpoint", name))
			}
			args = append(args, "-e", ServiceURLEnv(name)+"="+svc.Endpoint)
		case modeHelper:
			if svc.Endpoint == "" {
				return fail(fmt.Errorf("service %q: helper mode requires an endpoint", name))
			}
			args = append(args, "-e", HelperEnv(name, "ENDPOINT")+"="+svc.Endpoint)
		case modeFile:
			if !tmpfsChecked {
				if err := b.checkScratch(step.ScratchDir); err != nil {
					return fail(err)
				}
				tmpfsChecked = true
			}
			path, err := b.writeSecretFile(ctx, step.ScratchDir, name)
			if err != nil {
				return fail(err)
			}
			files = append(files, path)
			args = append(args, "-v", path+":"+ContainerSecretsDir+"/"+name+":ro")
			// Deliberately noisy: drift from proxy mode must stay visible.
			b.logger.WarnContext(ctx, "file-mode credential: degraded raw-token path (explicit opt-in)",
				"node", step.NodeID, "service", name)
		default:
			// The loader rejects unknown modes; fail closed anyway.
			return fail(fmt.Errorf("service %q: unknown credential mode %q", name, svc.Mode))
		}
	}
	var teardown func(ctx context.Context) error
	if len(files) > 0 {
		teardown = func(ctx context.Context) error {
			var errs []error
			for _, f := range files {
				if err := b.shred(f); err != nil {
					errs = append(errs, fmt.Errorf("shred %s: %w", f, err))
				}
			}
			return errors.Join(errs...)
		}
	}
	return Contribution{Args: args, Teardown: teardown}, nil
}

// checkScratch refuses a scratch dir that is not verifiably tmpfs-backed: the
// raw token must never touch disk.
func (b *CredentialBroker) checkScratch(dir string) error {
	if dir == "" {
		return errors.New("file-mode credentials require a per-step scratch dir")
	}
	ok, err := b.isTmpfs(dir)
	if err != nil {
		return fmt.Errorf("verify scratch dir is tmpfs-backed: %w", err)
	}
	if !ok {
		return fmt.Errorf("scratch dir %s is not tmpfs-backed; refusing to write a raw credential file", dir)
	}
	return nil
}

// writeSecretFile resolves the service's token host-side and writes it as the
// 0600 mount source. This is the only call site of Secret.reveal. Once the
// write has been attempted, any failure — a partial write on a size-limited
// tmpfs (ENOSPC), a failed chmod — shreds whatever landed in the scratch dir
// before returning, so a token file that never reached the caller's teardown
// list still cannot outlive the failed Prepare.
func (b *CredentialBroker) writeSecretFile(ctx context.Context, scratchDir, name string) (string, error) {
	if b.resolver == nil {
		return "", fmt.Errorf("service %q: file mode needs a credentials.resolver and none is configured", name)
	}
	tok, err := b.resolver.GetToken(ctx, name)
	if err != nil {
		return "", fmt.Errorf("service %q: %w", name, err)
	}
	path := filepath.Join(scratchDir, name)
	shredPartial := func() {
		if serr := b.shred(path); serr != nil {
			b.logger.WarnContext(ctx, "shred after failed token write", "service", name, "err", serr)
		}
	}
	if err := b.writeFile(path, tok.reveal(), 0o600); err != nil {
		shredPartial()
		return "", fmt.Errorf("service %q: write token file: %w", name, err)
	}
	// WriteFile applies the mode only at creation; a leftover file must not
	// widen it.
	if err := os.Chmod(path, 0o600); err != nil {
		shredPartial()
		return "", fmt.Errorf("service %q: restrict token file: %w", name, err)
	}
	return path, nil
}

// shredFile overwrites the file's full length with zeros, syncs, and removes
// it. A file already gone counts as shredded — there is nothing left to leak.
func shredFile(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	var errs []error
	if _, err := f.WriteAt(make([]byte, info.Size()), 0); err != nil {
		errs = append(errs, fmt.Errorf("overwrite: %w", err))
	}
	if err := f.Sync(); err != nil {
		errs = append(errs, fmt.Errorf("sync: %w", err))
	}
	if err := f.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := os.Remove(path); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
