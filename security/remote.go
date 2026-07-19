package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// RemoteBinding tells the box where its repository lives and how to trust
// that answer (requirement 14a0498eb362). It contributes environment only:
// the gateway clone URL (the configured prefix plus the step's resolved repo
// input, suffixed ".git") and the host-key policy material. The actual clone
// happens inside the box, over the identity binding's forwarded agent.
//
// Host-key policy is exactly one of three modes, with no silent default:
//
//   - pinned (host_key_file): the gateway's public host-key line is read
//     host-side at Prepare time and exported; the box installs it and
//     connects with StrictHostKeyChecking=yes. Fail closed — an unreadable or
//     empty key file fails the step before launch.
//   - tofu (tofu: true): sandbox-only explicit opt-in; the box uses
//     accept-new. Mutually exclusive with pinned.
//   - neither: abort with a clear error before any network contact.
//
// Everything trust-relevant at the remote — push validation, credential
// holding, PR mediation — is the user's gate service behind the git URL;
// faber only ever sees ssh://git@… and never holds the forge credential.
//
// Deferred gateless seam (requirement 18e9f712c810), first pass: remote: may
// be absent and repo-less steps skip cloning — the binding contributes
// nothing at all; terminal outputs live in the step's typed result and the
// egress lock remains the only enforced boundary. No further behavior exists.
type RemoteBinding struct {
	logger *slog.Logger
}

// NewRemoteBinding constructs the remote binding.
func NewRemoteBinding(logger *slog.Logger) *RemoteBinding {
	return &RemoteBinding{logger: childLogger(logger, "remote-binding")}
}

// Name implements Binding.
func (b *RemoteBinding) Name() string { return "remote" }

// Prepare derives the clone URL and host-key material. When the step has no
// repo input or no remote is configured, it contributes nothing (the box's
// clone phase is skipped by contract).
func (b *RemoteBinding) Prepare(_ context.Context, step StepSpec) (Contribution, error) {
	r := step.Remote
	if r == nil || step.Repo == "" {
		return Contribution{}, nil
	}
	if r.URL == "" {
		return Contribution{}, errors.New("remote configured without a url")
	}
	// The repo value is a resolved step input — possibly an upstream box's
	// output — spliced into the clone URL. It must be a plain repository
	// path: no traversal, no URL metacharacters, no whitespace or control
	// bytes that could re-shape the URL the box clones and pushes.
	if err := checkRepoShape(step.Repo); err != nil {
		return Contribution{}, fmt.Errorf("repo input %q: %w", step.Repo, err)
	}
	args := []string{"-e", EnvRemoteURL + "=" + cloneURL(r.URL, step.Repo)}
	switch {
	case r.HostKeyFile != "" && r.TOFU:
		// The loader rejects this; fail closed anyway rather than pick one.
		return Contribution{}, errors.New("both host_key_file and tofu configured; exactly one host-key mode is allowed")
	case r.HostKeyFile != "":
		line, err := readHostKeyLine(r.HostKeyFile)
		if err != nil {
			return Contribution{}, err
		}
		args = append(args, "-e", EnvHostKey+"="+line)
	case r.TOFU:
		args = append(args, "-e", EnvHostKeyTOFU+"=1")
	default:
		return Contribution{}, errors.New("no host-key policy configured: set host_key_file (pinned, fail closed) or tofu (sandbox-only opt-in)")
	}
	return Contribution{Args: args}, nil
}

// cloneURL splices the resolved repo input into the gateway URL prefix.
func cloneURL(prefix, repo string) string {
	return strings.TrimSuffix(prefix, "/") + "/" + repo + ".git"
}

// repoShape is the closed grammar for a spliced repo path: slash-separated
// segments of [A-Za-z0-9._-], no leading slash, no empty segment.
var repoShape = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// checkRepoShape rejects a repo value that could re-shape the clone URL or
// climb the gateway's path space.
func checkRepoShape(repo string) error {
	if !repoShape.MatchString(repo) {
		return fmt.Errorf("not a plain repository path (segments of [A-Za-z0-9._-] separated by '/')")
	}
	for _, seg := range strings.Split(repo, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("path segment %q is not allowed", seg)
		}
	}
	return nil
}

// readHostKeyLine reads the pinned gateway host key host-side: a trimmed
// single line. Empty, unreadable, or multi-line content fails the step before
// launch — a bad pin must never degrade to an in-box guess.
func readHostKeyLine(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read host key file: %w", err)
	}
	line := strings.TrimSpace(string(raw))
	if line == "" {
		return "", fmt.Errorf("host key file %s is empty", path)
	}
	if strings.ContainsAny(line, "\r\n") {
		return "", fmt.Errorf("host key file %s must contain a single host-key line", path)
	}
	return line, nil
}
