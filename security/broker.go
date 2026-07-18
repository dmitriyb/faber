package security

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
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
// helper), the stdin payload key and the /run/secrets/<name> file the box
// writes (file) — so a name carrying ':', '=', '/', or spaces must fail closed
// here too, naming the service, rather than surface as an opaque error mid-run.
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
//     (it stays in host RAM) and delivered into a container tmpfs over the
//     container's stdin — never a host file, never env (env leaks into docker
//     inspect, child processes, and crash dumps), never argv, never an image
//     layer, never the journal. The binding emits "--tmpfs /run/secrets" once
//     and carries the resolved tokens on its Contribution.Secrets; the runner
//     streams them on stdin and faber-box writes each 0600 file that dies with
//     the container — no host tmpfs, no shred.
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
}

// NewCredentialBroker wires the broker to the resolver seam. resolver may be
// nil when no credentials.resolver is configured; only file mode needs it.
func NewCredentialBroker(resolver Resolver, logger *slog.Logger) *CredentialBroker {
	return &CredentialBroker{
		resolver: resolver,
		logger:   childLogger(logger, "credential-broker"),
	}
}

// Name implements Binding.
func (b *CredentialBroker) Name() string { return "credentials" }

// Prepare walks the step's declared services in sorted name order (the
// fragment stays deterministic) and appends each handle's flags. File mode
// resolves each token host-side into the Contribution's Secrets map and emits
// one "--tmpfs <ContainerSecretsDir>" (once, regardless of how many file-mode
// services); the tokens themselves never enter Args and leave the module only
// as the encoded Assembled.SecretsStdin payload. Any resolver failure fails
// the step before a container exists — there is no host file to write and none
// to clean, so file mode contributes no teardown. Errors name the binding and
// service, never secret content — the Secret type makes that structural.
func (b *CredentialBroker) Prepare(ctx context.Context, step StepSpec) (Contribution, error) {
	if len(step.Services) == 0 {
		return Contribution{}, nil
	}
	var args []string
	var secrets map[string]Secret
	for _, name := range slices.Sorted(maps.Keys(step.Services)) {
		if !serviceNamePattern.MatchString(name) {
			// The loader rejects these; fail closed anyway for all modes.
			return Contribution{}, fmt.Errorf("service %q: invalid name (must match %s)", name, serviceNamePattern)
		}
		svc := step.Services[name]
		switch svc.Mode {
		case modeProxy:
			if svc.Endpoint == "" {
				return Contribution{}, fmt.Errorf("service %q: proxy mode requires an endpoint", name)
			}
			args = append(args, "-e", ServiceURLEnv(name)+"="+svc.Endpoint)
		case modeHelper:
			if svc.Endpoint == "" {
				return Contribution{}, fmt.Errorf("service %q: helper mode requires an endpoint", name)
			}
			args = append(args, "-e", HelperEnv(name, "ENDPOINT")+"="+svc.Endpoint)
		case modeFile:
			tok, err := b.resolveToken(ctx, name)
			if err != nil {
				return Contribution{}, err
			}
			if secrets == nil {
				secrets = map[string]Secret{}
				// One tmpfs for the whole secrets dir, emitted once regardless of
				// how many file-mode services — RAM in the container, no host file.
				args = append(args, "--tmpfs", ContainerSecretsDir)
			}
			secrets[name] = tok
			// Deliberately noisy: drift from proxy mode must stay visible.
			b.logger.WarnContext(ctx, "file-mode credential: degraded raw-token path (explicit opt-in)",
				"node", step.NodeID, "service", name)
		default:
			// The loader rejects unknown modes; fail closed anyway.
			return Contribution{}, fmt.Errorf("service %q: unknown credential mode %q", name, svc.Mode)
		}
	}
	return Contribution{Args: args, Secrets: secrets}, nil
}

// resolveToken invokes the user resolver host-side for one file-mode service.
// The token stays in host RAM as an opaque Secret; it is unwrapped only later,
// once, by encodeSecretsPayload. A nil resolver or a resolver failure fails
// the step, naming the service but never the token content.
func (b *CredentialBroker) resolveToken(ctx context.Context, name string) (Secret, error) {
	if b.resolver == nil {
		return Secret{}, fmt.Errorf("service %q: file mode needs a credentials.resolver and none is configured", name)
	}
	tok, err := b.resolver.GetToken(ctx, name)
	if err != nil {
		return Secret{}, fmt.Errorf("service %q: %w", name, err)
	}
	return tok, nil
}
