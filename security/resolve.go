package security

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmitriyb/faber/config"
)

// KeyLocator finds a host key whose fingerprint matches and returns the opaque
// keySource the IdentityBinding's AddKey loads. It reads only public material
// (agent listings, *.pub files, token metadata) — never a private key into
// faber's memory.
type KeyLocator interface {
	Locate(ctx context.Context, fingerprint string) (keySource string, err error)
}

// ResolveIdentity turns a step's declared identity into the keySource the
// ephemeral agent will load, applying explicit-path precedence:
//
//  1. def.Key is a path (anything not a SHA256: fingerprint) → returned
//     verbatim, no registry read and no locator call. Today's behavior exactly,
//     so every path-form config is byte-identical.
//  2. def.Key is a SHA256:… fingerprint → located directly, skipping the
//     role→fingerprint hop.
//  3. def.Key empty → role looked up in the registry, then its fingerprint
//     located.
//
// The second return value is the expected fingerprint the resolution pinned:
// the SHA256:… value the located key MUST carry, so the binding can verify the
// loaded key post-hoc. It is "" only for the explicit-path branch, where no
// fingerprint is known and the caller trusts the path verbatim (today's
// behavior); every fingerprint-driven branch returns the pinned value so the
// binding can fail closed if the agent ends up holding a different key.
//
// A role absent from the registry, or a fingerprint no local key matches, is a
// clear error naming the role and (where known) the fingerprint.
func ResolveIdentity(ctx context.Context, reg Registry, loc KeyLocator, role string, def config.IdentityDef) (keySource, fingerprint string, err error) {
	// Explicit path wins: any non-fingerprint value is used as-is. No
	// fingerprint is known for this branch, so verification is skipped.
	if def.Key != "" && !validFingerprint(def.Key) {
		return def.Key, "", nil
	}

	fingerprint = def.Key
	if fingerprint == "" {
		entry, ok := reg[role]
		if !ok {
			return "", "", fmt.Errorf("identity %q: role not in registry (register it with `faber add-key --role %s --fingerprint SHA256:…`)", role, role)
		}
		fingerprint = entry.Fingerprint
	}

	if loc == nil {
		return "", "", fmt.Errorf("identity %q: no key locator configured to resolve fingerprint %s", role, fingerprint)
	}
	src, err := loc.Locate(ctx, fingerprint)
	if err != nil {
		return "", "", fmt.Errorf("identity %q: no local key matches fingerprint %s: %w", role, fingerprint, err)
	}
	if src == "" {
		return "", "", fmt.Errorf("identity %q: no local key matches fingerprint %s", role, fingerprint)
	}
	return src, fingerprint, nil
}

// fingerprintHeld reports whether any of the agent's key listing lines carries
// the fingerprint fp. Each line is an `ssh-add -l` fingerprint line
// ("256 SHA256:… comment (TYPE)"); parseKeyLine extracts its SHA256 field.
func fingerprintHeld(lines []string, fp string) bool {
	for _, line := range lines {
		if got, _ := parseKeyLine(line); got == fp {
			return true
		}
	}
	return false
}

// commandFunc is the exec seam the real locator shells through; tests
// substitute a fake so no ssh binaries are needed. It returns combined stdout
// (stderr folded into the error) — the two commands it runs, `ssh-add -l` and
// `ssh-keygen -lf`, print only public fingerprint lines, never key material.
type commandFunc func(ctx context.Context, name string, args ...string) (stdout string, err error)

// resident is one YubiKey resident credential: its fingerprint and an opaque
// handle. NOTE: a resident credential is loaded with `ssh-add -K` (download all
// resident keys from an attached token), not `ssh-add <path>`. The current
// AgentController.AddKey runs `ssh-add <keySource>`, treating the source as a
// positional file path, so it cannot load a resident handle. This source is
// therefore not loadable by the built-in binding today; defaultResidentKeys
// returns none, so nothing here reaches AddKey. Wiring residents means teaching
// AddKey the `ssh-add -K` invocation as well.
type resident struct {
	fingerprint string
	keySource   string
}

// keyLocator is the production KeyLocator. It searches, first match wins:
// running ssh-agent, then ~/.ssh/*.pub (sorted), then YubiKey resident keys.
// Every seam is injectable so the search order is unit-testable without real
// binaries or a real token.
type keyLocator struct {
	logger *slog.Logger
	// sshDir is the directory scanned for *.pub keys (~/.ssh by default).
	sshDir string
	// run shells ssh-add / ssh-keygen; agentSocket, when set, is exported as
	// SSH_AUTH_SOCK so `ssh-add -l` queries the running agent.
	run         commandFunc
	agentSocket string
	// residents enumerates attached-token resident keys; nil = none.
	residents func(ctx context.Context) ([]resident, error)
}

// NewKeyLocator returns the real locator over ssh-add/ssh-keygen and the
// user's ~/.ssh, honoring the live SSH_AUTH_SOCK for the running-agent hop.
func NewKeyLocator(logger *slog.Logger) KeyLocator {
	sshDir := ""
	if home, err := os.UserHomeDir(); err == nil {
		sshDir = filepath.Join(home, ".ssh")
	}
	l := &keyLocator{
		logger:      childLogger(logger, "key-locator"),
		sshDir:      sshDir,
		agentSocket: os.Getenv(EnvSSHAuthSock),
	}
	l.run = l.execCommand
	l.residents = defaultResidentKeys
	return l
}

// execCommand is the default commandFunc: a direct exec with a minimal
// environment (PATH/HOME plus the agent socket for ssh-add -l). Never through a
// shell.
func (l *keyLocator) execCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	env := []string{}
	for _, key := range []string{"PATH", "HOME"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	if l.agentSocket != "" {
		env = append(env, EnvSSHAuthSock+"="+l.agentSocket)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// Locate implements KeyLocator with the fixed three-source order.
func (l *keyLocator) Locate(ctx context.Context, fingerprint string) (string, error) {
	if src := l.fromAgent(ctx, fingerprint); src != "" {
		return src, nil
	}
	if src := l.fromPubDir(ctx, fingerprint); src != "" {
		return src, nil
	}
	if src := l.fromResidents(ctx, fingerprint); src != "" {
		return src, nil
	}
	return "", fmt.Errorf("no key on the running agent, in %s, or on an attached token matches %s", l.sshDir, fingerprint)
}

// fromAgent parses `ssh-add -l` and returns the matching line's comment when it
// names a readable file; a match whose comment is not a usable path is skipped
// (the private key still cannot be handed to ssh-add).
func (l *keyLocator) fromAgent(ctx context.Context, fingerprint string) string {
	if l.run == nil {
		return ""
	}
	out, err := l.run(ctx, "ssh-add", "-l")
	if err != nil {
		// Exit 1 is "the agent has no identities" — not an error, just no
		// match here. Any other failure (no agent, no ssh-add) is logged at
		// debug and treated as no match so the next source is tried.
		l.logger.DebugContext(ctx, "ssh-add -l unavailable", "err", err)
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		fp, comment := parseKeyLine(line)
		if fp != fingerprint {
			continue
		}
		if comment != "" && regularFileExists(comment) {
			return comment
		}
		l.logger.DebugContext(ctx, "agent holds the key but its comment is not a readable path; trying other sources",
			"fingerprint", fingerprint, "comment", comment)
		return ""
	}
	return ""
}

// fromPubDir walks ~/.ssh/*.pub in sorted order, reads each public key's
// fingerprint via `ssh-keygen -lf`, and on a match returns the private
// counterpart (the same path without .pub) when it exists and is readable.
func (l *keyLocator) fromPubDir(ctx context.Context, fingerprint string) string {
	if l.sshDir == "" || l.run == nil {
		return ""
	}
	pubs, err := filepath.Glob(filepath.Join(l.sshDir, "*.pub"))
	if err != nil {
		l.logger.DebugContext(ctx, "glob ~/.ssh/*.pub", "err", err)
		return ""
	}
	sort.Strings(pubs)
	for _, pub := range pubs {
		out, err := l.run(ctx, "ssh-keygen", "-lf", pub)
		if err != nil {
			l.logger.DebugContext(ctx, "ssh-keygen -lf failed", "pub", pub, "err", err)
			continue
		}
		fp, _ := parseKeyLine(out)
		if fp != fingerprint {
			continue
		}
		priv := strings.TrimSuffix(pub, ".pub")
		if regularFileExists(priv) {
			return priv
		}
		l.logger.DebugContext(ctx, "public key matched but its private counterpart is not readable",
			"pub", pub, "private", priv)
	}
	return ""
}

// fromResidents matches an attached token's resident keys, returning the
// resident-key handle. That handle is loaded with `ssh-add -K`, which the
// current AgentController.AddKey does not issue (see the resident type), so
// this source is unreachable in the built-in binding until AddKey learns that
// invocation; defaultResidentKeys returns none by default.
func (l *keyLocator) fromResidents(ctx context.Context, fingerprint string) string {
	if l.residents == nil {
		return ""
	}
	res, err := l.residents(ctx)
	if err != nil {
		l.logger.DebugContext(ctx, "enumerate resident keys", "err", err)
		return ""
	}
	for _, r := range res {
		if r.fingerprint == fingerprint {
			return r.keySource
		}
	}
	return ""
}

// defaultResidentKeys is the no-token default: faber does not touch a hardware
// token during resolution unless one is wired in. Enumerating resident keys
// requires a PIN-gated download that would surprise a run, so the built-in
// locator reports none and leaves that source to explicit configuration.
func defaultResidentKeys(context.Context) ([]resident, error) { return nil, nil }

// parseKeyLine extracts the fingerprint and comment from one ssh-add/ssh-keygen
// listing line, e.g. "256 SHA256:abc… user@host (ED25519)". The comment is
// everything between the fingerprint and the trailing "(TYPE)" token; a line
// with no SHA256 field yields "", "".
func parseKeyLine(line string) (fingerprint, comment string) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return "", ""
	}
	fpIdx := -1
	for i, f := range fields {
		if strings.HasPrefix(f, "SHA256:") {
			fpIdx = i
			fingerprint = f
			break
		}
	}
	if fpIdx < 0 {
		return "", ""
	}
	rest := fields[fpIdx+1:]
	// Drop a trailing "(TYPE)" token if present.
	if n := len(rest); n > 0 && strings.HasPrefix(rest[n-1], "(") && strings.HasSuffix(rest[n-1], ")") {
		rest = rest[:n-1]
	}
	return fingerprint, strings.Join(rest, " ")
}

// regularFileExists reports whether path is an existing regular file. It only
// stats — it never opens the file — so faber never reads a private key into
// memory when confirming a key source exists; ssh-add surfaces an actual
// permission problem loudly when it later loads the path.
func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
