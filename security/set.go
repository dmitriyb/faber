package security

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"
)

// teardownGrace bounds the detached teardown context: a user abort must still
// kill agents, but teardown cannot hang forever. File mode leaves no host-side
// residue — its tokens live only in the container tmpfs and die with it.
const teardownGrace = 30 * time.Second

// Assembled is one step attempt's security surface, ready for infra: Args is
// the ordered docker-run argv fragment ContainerRunner splices verbatim
// (RunSpec.Bindings), SecretsStdin is the framed JSON file-mode secrets
// payload the runner streams on the container's stdin (nil = no file-mode
// secret, no -i), and Teardown is the post-run hook that must bracket the Run
// call — infra runs no hooks itself. Teardown is never nil, runs every
// registered hook in reverse setup order on a context detached from step
// cancellation, and joins all errors; call it exactly once, after the
// container has exited (any status, or kill-on-cancel).
type Assembled struct {
	Args         []string
	SecretsStdin []byte
	Teardown     func(ctx context.Context) error
}

// encodeSecretsPayload is the sole reveal() caller: it encodes the merged
// file-mode tokens into one JSON object {"<name>":"<base64(token)>", ...}.
// Sorted keys keep the bytes deterministic; base64 (std encoding) lets
// arbitrary token bytes survive JSON transit intact. This is the one place a
// Secret is unwrapped — base64 of the token leaves the type here and nowhere
// else.
func encodeSecretsPayload(secrets map[string]Secret) ([]byte, error) {
	m := make(map[string]string, len(secrets))
	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		m[name] = base64.StdEncoding.EncodeToString(secrets[name].reveal())
	}
	return json.Marshal(m)
}

// BindingSet composes the per-step binding contributions into the fragment
// and lifecycle hooks. The composition order is fixed at construction —
// network, remote, identity, credentials, runtime — and each binding's args
// are internally ordered, so the same resolved step always yields the same
// fragment byte for byte (no map is iterated anywhere in assembly).
//
// The set is per-step and stateless across steps: no pooling, no cached
// secrets or agents. Parallel steps compose disjoint resources (private
// socket dirs, per-attempt scratch files) and are safe to assemble
// concurrently. It also owns the isolation runtime knob (requirement
// 41bb811a34e8) as its trivial last member.
type BindingSet struct {
	bindings []Binding
	logger   *slog.Logger
}

// NewBindingSet wires the fixed composition order. All four bindings are
// required (each contributes nothing when its slice of config is absent from
// a step); the runtime knob is appended internally.
func NewBindingSet(network *NetworkBinding, remote *RemoteBinding, identity *IdentityBinding, credentials *CredentialBroker, logger *slog.Logger) *BindingSet {
	return &BindingSet{
		bindings: []Binding{network, remote, identity, credentials, runtimeBinding{}},
		logger:   childLogger(logger, "binding-set"),
	}
}

// NewBindingSetWithoutCredentials composes the observe-only surface — network,
// remote, and identity (plus the runtime knob) — with no credential broker at
// all. The interactive re-entry shell uses it: that shell observes a failed
// step, it never runs the agent, and it cannot materialize a stdin secrets
// payload anyway (re-entry replaces the box sequencer with a raw shell). So it
// carries no credentials — secrets are neither resolved (no resolver/GetToken
// call, no minted-token side effect) nor streamed. Network/remote/identity stay
// so clone and observe still work; an operator who needs a secret sets it by
// hand.
func NewBindingSetWithoutCredentials(network *NetworkBinding, remote *RemoteBinding, identity *IdentityBinding, logger *slog.Logger) *BindingSet {
	return &BindingSet{
		bindings: []Binding{network, remote, identity, runtimeBinding{}},
		logger:   childLogger(logger, "binding-set"),
	}
}

// Prepare assembles one step attempt: setup hooks run in composition order,
// fail-fast across bindings. On a binding's failure the already-prepared
// bindings are unwound in reverse (every hook attempted, errors logged) and
// the error names the failing binding — no container was launched, so
// nothing else exists to clean. Retry is a step-level concern: each attempt
// re-prepares from scratch (fresh agent, fresh resolver invocation, fresh
// secrets payload) against a fresh StepSpec.
func (s *BindingSet) Prepare(ctx context.Context, step StepSpec) (Assembled, error) {
	var args []string
	var undo []namedTeardown
	var secrets map[string]Secret // only the credentials binding sets any
	for _, b := range s.bindings {
		c, err := b.Prepare(ctx, step)
		if err != nil {
			if terr := s.teardownAll(ctx, undo); terr != nil {
				s.logger.WarnContext(ctx, "teardown after failed assembly",
					"node", step.NodeID, "failed_binding", b.Name(), "err", terr)
			}
			return Assembled{}, fmt.Errorf("security: binding %s: %w", b.Name(), err)
		}
		args = append(args, c.Args...)
		for name, tok := range c.Secrets {
			if secrets == nil {
				secrets = map[string]Secret{}
			}
			secrets[name] = tok
		}
		if c.Teardown != nil {
			undo = append(undo, namedTeardown{name: b.Name(), fn: c.Teardown})
		}
	}
	var payload []byte
	if len(secrets) > 0 {
		var err error
		if payload, err = encodeSecretsPayload(secrets); err != nil {
			if terr := s.teardownAll(ctx, undo); terr != nil {
				s.logger.WarnContext(ctx, "teardown after secrets encode failed", "node", step.NodeID, "err", terr)
			}
			return Assembled{}, fmt.Errorf("security: binding credentials: %w", err)
		}
	}
	return Assembled{
		Args:         args,
		SecretsStdin: payload,
		Teardown:     func(ctx context.Context) error { return s.teardownAll(ctx, undo) },
	}, nil
}

// namedTeardown pairs a binding's teardown hook with its name for error
// attribution.
type namedTeardown struct {
	name string
	fn   func(ctx context.Context) error
}

// teardownAll runs the undo stack last-to-first on a context detached from
// step cancellation with a short deadline, calls every hook even when earlier
// ones fail, and joins the errors — a failed teardown hook must not skip
// killing the agent, and a user abort must not skip either.
func (s *BindingSet) teardownAll(ctx context.Context, undo []namedTeardown) error {
	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownGrace)
	defer cancel()
	var errs []error
	for i := len(undo) - 1; i >= 0; i-- {
		if err := undo[i].fn(tctx); err != nil {
			errs = append(errs, fmt.Errorf("security: teardown %s: %w", undo[i].name, err))
		}
	}
	return errors.Join(errs...)
}

// runtimeBinding is the isolation runtime knob (requirement 41bb811a34e8): an
// optional per-template (or workflow-default) value mapped to
// --runtime=<value> — e.g. runsc for gVisor on Linux. Last in the fragment,
// it changes only the argv, never the image: the same tag runs under either
// runtime. Absent means the platform default and contributes nothing.
type runtimeBinding struct{}

func (runtimeBinding) Name() string { return "runtime" }

func (runtimeBinding) Prepare(_ context.Context, step StepSpec) (Contribution, error) {
	if step.Runtime == "" {
		return Contribution{}, nil
	}
	return Contribution{Args: []string{"--runtime=" + step.Runtime}}, nil
}
