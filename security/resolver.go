package security

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/dmitriyb/faber/infra"
)

// Resolver is the credential-delegation seam: get_token(service) as a typed
// interface. Implementations run host-side only — never inside a container.
type Resolver interface {
	// GetToken resolves the named service's credential to an opaque Secret.
	GetToken(ctx context.Context, service string) (Secret, error)
}

// ExecResolver is the production Resolver: it shells the user's opaque
// credentials.resolver command through infra's CommandRunner with argv
// [resolver, service], stdin closed, and types the captured stdout (trailing
// newline trimmed) as a Secret. What the command does — OS keychain, an
// encrypted vault CLI, an env lookup, a static file — is invisible by design.
//
// There is no caching: each step attempt that needs a token invokes the
// resolver afresh, which is also the whole first-pass refresh story for the
// deferred secret-expiry seam (requirement 0157d30de15f) — an expired
// credential fails its step, and standard retry re-resolves.
type ExecResolver struct {
	path   string
	runner infra.CommandRunner
}

// NewExecResolver wires the user's resolver command path (the
// credentials.resolver declaration) to the opaque-command runner.
func NewExecResolver(path string, runner infra.CommandRunner) *ExecResolver {
	return &ExecResolver{path: path, runner: runner}
}

// GetToken implements Resolver. Errors name the service but never any output
// content: infra's CommandRunner already guarantees its errors and logs carry
// neither stdout bytes nor the argument list.
func (r *ExecResolver) GetToken(ctx context.Context, service string) (Secret, error) {
	if r.path == "" {
		return Secret{}, errors.New("no credentials.resolver configured")
	}
	res, err := r.runner.Run(ctx, infra.CmdSpec{Path: r.path, Args: []string{service}})
	if err != nil {
		return Secret{}, fmt.Errorf("resolver for service %q: %w", service, err)
	}
	tok := bytes.TrimRight(res.Stdout, "\r\n")
	if len(tok) == 0 {
		return Secret{}, fmt.Errorf("resolver for service %q returned no token", service)
	}
	return NewSecret(tok), nil
}
