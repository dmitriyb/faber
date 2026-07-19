// Package box is the in-container half of the agent module: the phase
// sequencer the faber-box binary runs as every step container's entry
// program. It owns the fixed, engine-defined phase order — skills link, env
// contract, delegated secrets, host-key policy, gateway clone, signing config,
// context hook, prelude hook, agent invocation, result emission — with
// fail-stop between phases and one attempt record on every exit path. There is
// no in-container DAG and nothing here is configurable per template.
//
// The package holds no resolver and fetches no secret (the host delegated
// handles before the container existed), applies no policy, and never
// retries: a step is atomic, and the host's failure policy re-runs the whole
// box.
package box

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
)

// Box is one step attempt's in-container state, constructed in main from the
// process environment and injected seams — the whole sequence unit-tests as a
// plain value with a fake runner.
type Box struct {
	Env    *BoxEnv
	Runner CmdRunner
	Log    *slog.Logger

	// Stdin is the box's standard input, read exactly once by the secrets
	// phase when FABER_SECRETS_STDIN=1 (the file-mode credential payload).
	// Injectable for tests; the binary wires os.Stdin. When the flag is unset
	// the phase never touches it.
	Stdin io.Reader

	// Environ is the child environment for hooks and the agent: the process
	// environment plus everything the setup phases export (secrets, git ssh
	// policy). Kept on the Box instead of os.Setenv so the sequencer holds no
	// global state.
	Environ []string

	// Workdir is set by the clone phase: the gateway clone, or a scratch
	// directory on a gateless step.
	Workdir string

	// Bundle is set after the prelude phase.
	Bundle *Bundle

	// Timing holds the per-phase clocks emitted into the attempt record.
	Timing map[string]time.Duration

	// hookDeclared records whether any hook file existed; a hook-less
	// template gets a synthesized context bundle.
	hookDeclared bool
}

// New constructs a Box. environ is the process environment (os.Environ() in
// the binary); logger nil means discard.
func New(env *BoxEnv, runner CmdRunner, environ []string, logger *slog.Logger) *Box {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Box{
		Env:     env,
		Runner:  runner,
		Log:     logger.With("component", "box"),
		Stdin:   os.Stdin,
		Environ: append([]string(nil), environ...),
		Timing:  map[string]time.Duration{},
	}
}

// phase is one row of the fixed order.
type phase struct {
	name string
	run  func(*Box, context.Context) error
}

// phases is the engine-owned sequence — the spec's fixed box phase order is
// literally this slice. It is package data, not configuration: no template,
// env value, or flag reorders it.
var phases = []phase{
	{"skills", (*Box).linkSkills},
	{"env", (*Box).checkEnv},
	{"secrets", (*Box).loadSecrets},
	{"hostkey", (*Box).applyHostKeyPolicy},
	{"clone", (*Box).clone},
	{"signing", (*Box).configureSigning},
	{"context", (*Box).runContextHook},
	{"prelude", (*Box).runPreludeHook},
	{"agent", (*Box).runAgent},
	{"result", (*Box).emitResult},
}

// Main drives the fixed order with fail-stop between phases: no phase ever
// runs after a failed one, and every failure converges on the handoff funnel.
// The return value is the process exit code.
func Main(ctx context.Context, b *Box) int {
	if err := b.enterRunUser(ctx); err != nil {
		b.failStop(ctx, "preamble", err)
		return 1
	}
	for _, p := range phases {
		b.Log.InfoContext(ctx, "phase start", "phase", p.name)
		start := time.Now()
		err := p.run(b, ctx)
		b.Timing[p.name] = time.Since(start)
		if err != nil {
			b.failStop(ctx, p.name, err)
			return 1
		}
		b.Log.DebugContext(ctx, "phase done", "phase", p.name, "duration", b.Timing[p.name])
	}
	return 0
}

// enterRunUser is the privileged preamble (arch phase 0). The container starts
// as root because the writable mounts arrive root-owned; this chowns exactly
// those mounts to the run user, exports HOME, and drops privileges — setgroups
// to the single run group, setgid, setuid — so every phase below, and the
// untrusted agent above all, runs non-root. The toolset store paths stay
// root-owned and read-only. A box already running non-root, or with no run uid
// (a gateless local invocation with no root to drop), is a no-op. It is the
// only moment faber-box holds privilege.
func (b *Box) enterRunUser(ctx context.Context) error {
	if os.Getuid() != 0 || b.Env.RunUID == 0 {
		return nil
	}
	fail := func(step string, err error) error {
		return &boxError{Reason: contract.ReasonEnvContract, Detail: fmt.Sprintf("preamble: %s: %v", step, err)}
	}
	for _, dir := range []string{b.Env.WorkspaceDir, b.Env.BundleDir, "/tmp", contract.ContainerHome} {
		if err := os.Chown(dir, b.Env.RunUID, b.Env.RunGID); err != nil {
			return fail("chown "+dir, err)
		}
	}
	// The /run/secrets tmpfs is present only in file mode and mounted
	// root-owned by the binding's --tmpfs; chown it — but only when it exists —
	// so the secrets phase can write its 0600 files as the dropped run user.
	// Absent file mode there is no such mount and the chown set is unchanged.
	if _, err := os.Stat(b.Env.SecretsDir); err == nil {
		if err := os.Chown(b.Env.SecretsDir, b.Env.RunUID, b.Env.RunGID); err != nil {
			return fail("chown "+b.Env.SecretsDir, err)
		}
	}
	b.setEnv("HOME", contract.ContainerHome)
	if err := syscall.Setgroups([]int{b.Env.RunGID}); err != nil {
		return fail("setgroups", err)
	}
	if err := syscall.Setgid(b.Env.RunGID); err != nil {
		return fail("setgid", err)
	}
	if err := syscall.Setuid(b.Env.RunUID); err != nil {
		return fail("setuid", err)
	}
	b.Log.InfoContext(ctx, "dropped to run user", "uid", b.Env.RunUID, "gid", b.Env.RunGID)
	return nil
}

// boxError is a phase failure carrying the record fields the fail-stop funnel
// needs. Detail must be secret-free: engine prose plus, at most, the exit
// code — never an env value.
type boxError struct {
	Reason     string
	Detail     string
	ExitCode   int
	StderrTail string
}

func (e *boxError) Error() string {
	if e.ExitCode != 0 {
		return fmt.Sprintf("%s: %s (exit %d)", e.Reason, e.Detail, e.ExitCode)
	}
	return e.Reason + ": " + e.Detail
}

// failStop is the single failure funnel: it writes the structured handoff
// record plus a snapshot of the bundle directory into the mounted result
// directory (so container removal cannot lose them), then the failed attempt
// record carrying the handoff pointer. Errors here can only be logged — the
// host synthesizes a box-vanished record when no readable record remains.
func (b *Box) failStop(ctx context.Context, phaseName string, err error) {
	berr := asBoxError(phaseName, err)
	b.Log.ErrorContext(ctx, "phase failed", "phase", phaseName, "reason", berr.Reason, "exit_code", berr.ExitCode)

	dir := b.Env.ResultDir
	if dir == "" {
		b.Log.ErrorContext(ctx, "no result dir; handoff and record lost to the host")
		return
	}
	handoff := contract.Handoff{
		Phase:      phaseName,
		Reason:     berr.Reason,
		ExitCode:   berr.ExitCode,
		StderrTail: string(berr.StderrTail),
		Inputs:     b.Env.Inputs,
		Workdir:    b.Workdir,
	}
	// Slot-keyed inputs whenever the host declared the slot list: re-entry
	// feeds these straight back into the slot-named run contract. Without the
	// list the token-keyed map stands (Keying absent marks the old shape).
	if len(b.Env.Slots) > 0 {
		inputs := make(map[string]string, len(b.Env.Slots))
		for _, slot := range b.Env.Slots {
			if v, ok := b.Env.Inputs[contract.SlotToken(slot)]; ok {
				inputs[slot] = v
			}
		}
		handoff.Keying = contract.HandoffKeyingSlot
		handoff.Inputs = inputs
	}
	handoffRef := ""
	if werr := contract.WriteHandoffFile(dir, handoff); werr != nil {
		b.Log.ErrorContext(ctx, "write handoff", "err", werr)
	} else {
		handoffRef = contract.HandoffFile
	}
	b.snapshotBundle(ctx, dir)

	rec := contract.Result{
		Status: contract.StatusFailed,
		Error: &contract.ResultError{
			Reason:  berr.Reason,
			Detail:  berr.Detail,
			Handoff: handoffRef,
		},
		Timing:  b.Timing,
		Attempt: b.Env.Attempt,
	}
	if werr := contract.WriteResultFile(dir, rec); werr != nil {
		b.Log.ErrorContext(ctx, "write failed record", "err", werr)
	}
}

// asBoxError normalizes any phase error into the record fields; a plain error
// gets the phase name's default reason.
func asBoxError(phaseName string, err error) *boxError {
	var berr *boxError
	if errors.As(err, &berr) {
		return berr
	}
	reason := map[string]string{
		"env":     contract.ReasonEnvContract,
		"secrets": contract.ReasonSecrets,
		"hostkey": contract.ReasonHostKeyPolicy,
		"clone":   contract.ReasonCloneFailed,
		"signing": contract.ReasonSigning,
		"context": contract.ReasonHookFailed,
		"prelude": contract.ReasonHookFailed,
		"agent":   contract.ReasonAgentFailed,
		"result":  contract.ReasonResultWrite,
	}[phaseName]
	if reason == "" {
		reason = phaseName
	}
	return &boxError{Reason: reason, Detail: err.Error()}
}

// snapshotBundle copies the bundle directory beside the handoff record.
func (b *Box) snapshotBundle(ctx context.Context, resultDir string) {
	src := b.Env.BundleDir
	if src == "" {
		return
	}
	if _, err := os.Stat(src); err != nil {
		return
	}
	dst := filepath.Join(resultDir, filepath.FromSlash(contract.HandoffBundleDir))
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		b.Log.WarnContext(ctx, "snapshot bundle", "err", err)
	}
}

// setEnv sets key=val in the child environment (replacing any existing
// entry). Values may be delegated secrets: they are never logged.
func (b *Box) setEnv(key, val string) {
	prefix := key + "="
	for i, kv := range b.Environ {
		if strings.HasPrefix(kv, prefix) {
			b.Environ[i] = prefix + val
			return
		}
	}
	b.Environ = append(b.Environ, prefix+val)
}

// lookupEnv returns the value of key in the child environment (b.Environ), or
// "" when unset. It scans b.Environ the same way setEnv does — the box's own
// environment, not the process env, is authoritative for every phase (the
// preamble's HOME=/home/box lives only here, by the no-os.Setenv policy).
func (b *Box) lookupEnv(key string) string {
	prefix := key + "="
	for _, kv := range b.Environ {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

// linkSkills is the skills leg (phase 1): the one agent-specific translation,
// driven entirely by config so faber never hardcodes an agent's skills path. It
// joins the box environment's HOME — on the production drop path the preamble
// has already set HOME=/home/box (the writable tmpfs) via b.setEnv, so the link
// lands on the box's own scratch; on the no-drop local path (non-root or
// RunUID==0, e.g. the box-lifecycle tests running the binary as a plain
// process) HOME is whatever the caller/harness put in b.Environ. It reads
// b.Environ, never os.Getenv: the preamble mutates only b.Environ (the
// no-global-state policy), so the process HOME can diverge, and the agent and
// hooks below all resolve HOME from b.Environ too. It is a no-op when no skills
// leg was declared. os.Symlink, not a shell command: the image is shell-less.
// The target is the read-only engine mount; the link name is opaque agent
// config.
func (b *Box) linkSkills(ctx context.Context) error {
	if b.Env.SkillsLink == "" {
		return nil // no skills leg on this template
	}
	link := filepath.Join(b.lookupEnv("HOME"), b.Env.SkillsLink)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("skills: mkdir %s: %w", filepath.Dir(link), err)
	}
	if err := os.Symlink(contract.ContainerSkillsDir, link); err != nil {
		return fmt.Errorf("skills: symlink %s -> %s: %w", link, contract.ContainerSkillsDir, err)
	}
	return nil
}

// checkEnv is phase 2: the env contract. Every violation is collected, the
// bundle directory is created, and the result directory must be usable —
// without it no record could reach the host.
func (b *Box) checkEnv(ctx context.Context) error {
	if err := b.Env.validate(); err != nil {
		return &boxError{Reason: contract.ReasonEnvContract, Detail: err.Error()}
	}
	if err := os.MkdirAll(b.Env.ResultDir, 0o755); err != nil {
		return &boxError{Reason: contract.ReasonEnvContract, Detail: fmt.Sprintf("result dir: %v", err)}
	}
	if err := os.MkdirAll(b.Env.BundleDir, 0o755); err != nil {
		return &boxError{Reason: contract.ReasonEnvContract, Detail: fmt.Sprintf("bundle dir: %v", err)}
	}
	return nil
}

// loadSecrets is phase 3, in two steps. First, when FABER_SECRETS_STDIN=1
// (file mode delivered its tokens over stdin), it reads all of the box's stdin
// to EOF, JSON-decodes the single object {"<name>":"<base64(token)>", ...},
// base64-decodes each value, and writes /run/secrets/<name> at 0600 into the
// container tmpfs the preamble already chowned — a malformed payload or a
// decode error aborts with reason secrets. Stdin is read exactly once and only
// here; nothing earlier touches it and the headless agent never reads it, so
// faber closing stdin gives a clean EOF. Second (unchanged, and whether or not
// the stdin step ran): each regular file under the secrets directory is
// exported into the child environment under its uppercased basename. The
// values exist only in this process tree — never in a docker argv, a log
// record, or the handoff.
func (b *Box) loadSecrets(ctx context.Context) error {
	if b.Env.SecretsStdin {
		if err := b.materializeStdinSecrets(ctx); err != nil {
			return &boxError{Reason: contract.ReasonSecrets, Detail: err.Error()}
		}
	}
	entries, err := os.ReadDir(b.Env.SecretsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return &boxError{Reason: contract.ReasonSecrets, Detail: fmt.Sprintf("read secrets dir: %v", err)}
	}
	seen := map[string]string{} // env token -> first secret claiming it
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		key := contract.SlotToken(entry.Name())
		if first, dup := seen[key]; dup {
			return &boxError{Reason: contract.ReasonSecrets,
				Detail: fmt.Sprintf("secrets %q and %q both export the variable %s; the later would silently shadow the earlier", first, entry.Name(), key)}
		}
		seen[key] = entry.Name()
		// The same reserved-name rule the bundle sidecar obeys: a secret named
		// "path" or "home" would export over a runner- or engine-owned
		// variable in every hook and agent invocation. Validate rejects the
		// service name upstream; this is the in-box floor for hand-built
		// secrets dirs.
		if reservedSidecarKey(key) {
			return &boxError{Reason: contract.ReasonSecrets,
				Detail: fmt.Sprintf("secret %q would export the engine- or runner-owned variable %s; rename the credential service", entry.Name(), key)}
		}
		raw, err := os.ReadFile(filepath.Join(b.Env.SecretsDir, entry.Name()))
		if err != nil {
			return &boxError{Reason: contract.ReasonSecrets, Detail: fmt.Sprintf("read secret file %q: %v", entry.Name(), err)}
		}
		b.setEnv(key, strings.TrimSpace(string(raw)))
		b.Log.DebugContext(ctx, "secret exported", "key", key)
	}
	return nil
}

// materializeStdinSecrets reads the file-mode secrets payload from the box's
// stdin and writes each token as a 0600 file into the secrets tmpfs. It is the
// mirror of the host-side encodeSecretsPayload: read all of stdin, JSON-decode
// the single {"<name>":"<base64(token)>"} object, base64-decode each value,
// and write it. The raw token bytes never enter a log line or an error string.
func (b *Box) materializeStdinSecrets(ctx context.Context) error {
	if b.Stdin == nil {
		return errors.New("secrets payload signalled but the box has no stdin")
	}
	raw, err := io.ReadAll(b.Stdin)
	if err != nil {
		return fmt.Errorf("read stdin secrets payload: %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode stdin secrets payload: %v", err)
	}
	if err := os.MkdirAll(b.Env.SecretsDir, 0o700); err != nil {
		return fmt.Errorf("secrets dir: %v", err)
	}
	for name, b64 := range payload {
		// Defense in depth: the host emits only validated service names, but
		// the write joins name onto the tmpfs path — a traversal or nested
		// name must never land outside the secrets dir (where it would also
		// dodge the reserved-name export check below).
		if name == "" || name == "." || name == ".." || filepath.IsAbs(name) ||
			strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("secret name %q is not a single path segment", name)
		}
		tok, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fmt.Errorf("decode token for %q: not valid base64", name)
		}
		if err := os.WriteFile(filepath.Join(b.Env.SecretsDir, name), tok, 0o600); err != nil {
			return fmt.Errorf("write secret file %q: %v", name, err)
		}
	}
	b.Log.DebugContext(ctx, "stdin secrets materialized", "count", len(payload))
	return nil
}

// applyHostKeyPolicy is phase 4: pinned key material is installed into a
// known-hosts file with StrictHostKeyChecking=yes (fail closed); an explicit
// TOFU opt-in selects accept-new; an ssh remote with neither aborts before
// any network use. Non-ssh remotes (the local-path gateway of the sandbox
// and the tests) need no policy.
func (b *Box) applyHostKeyPolicy(ctx context.Context) error {
	switch {
	case b.Env.HostKey != "":
		file, err := b.writeKnownHosts(b.Env.HostKey)
		if err != nil {
			return &boxError{Reason: contract.ReasonHostKeyPolicy, Detail: err.Error()}
		}
		b.setEnv("GIT_SSH_COMMAND",
			"ssh -o UserKnownHostsFile="+file+" -o StrictHostKeyChecking=yes")
	case b.Env.TOFU:
		b.setEnv("GIT_SSH_COMMAND", "ssh -o StrictHostKeyChecking=accept-new")
	case isSSHRemote(b.Env.RemoteURL):
		return &boxError{
			Reason: contract.ReasonHostKeyPolicy,
			Detail: "ssh remote with neither pinned host key nor TOFU opt-in; refusing before any network use",
		}
	}
	return nil
}

// writeKnownHosts materializes the pinned host-key line. A bare public-key
// line (as read from the gateway's host key file) is prefixed with the
// remote's known-hosts host pattern — OpenSSH's "[host]:port" form on a
// non-default port; a full known-hosts line is written verbatim.
func (b *Box) writeKnownHosts(line string) (string, error) {
	if isKeyAlgorithm(strings.Fields(line)) {
		pattern := knownHostsPattern(b.Env.RemoteURL)
		if pattern == "" {
			return "", errors.New("pinned host key set but the remote URL names no host")
		}
		line = pattern + " " + line
	}
	f, err := os.CreateTemp("", "faber-known-hosts-*")
	if err != nil {
		return "", fmt.Errorf("write known-hosts: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write known-hosts: %v", err)
	}
	return f.Name(), nil
}

// isKeyAlgorithm reports whether the line's first token is a public-key
// algorithm (a bare key line) rather than a hostname.
func isKeyAlgorithm(fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	switch {
	case strings.HasPrefix(fields[0], "ssh-"),
		strings.HasPrefix(fields[0], "ecdsa-"),
		strings.HasPrefix(fields[0], "sk-"):
		return true
	}
	return false
}

// isSSHRemote reports whether the clone URL uses the ssh transport: an
// ssh:// scheme or the scp-like user@host:path form.
func isSSHRemote(url string) bool {
	if url == "" {
		return false
	}
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	if strings.Contains(url, "://") {
		return false
	}
	// scp-like: user@host:path (no scheme, a colon before the first slash).
	at := strings.Index(url, "@")
	colon := strings.Index(url, ":")
	slash := strings.Index(url, "/")
	return at >= 0 && colon > at && (slash < 0 || colon < slash)
}

// knownHostsPattern derives the known-hosts host field from the remote URL:
// the bare host, or OpenSSH's "[host]:port" form when an explicit non-default
// port is present (ssh-keygen -F looks keys up under exactly that pattern).
func knownHostsPattern(url string) string {
	host, port := remoteHostPort(url)
	if host == "" {
		return ""
	}
	if port != "" && port != "22" {
		return "[" + host + "]:" + port
	}
	return host
}

// remoteHostPort extracts the host and explicit port from an ssh:// or
// scp-like remote URL. The scp-like form has no port syntax.
func remoteHostPort(url string) (host, port string) {
	rest, isURL := strings.CutPrefix(url, "ssh://")
	if isURL {
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i]
		}
	} else {
		i := strings.Index(url, ":")
		if i < 0 {
			return "", ""
		}
		rest = url[:i]
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if !isURL {
		return rest, ""
	}
	// A bracketed IPv6 literal keeps its brackets around the host part.
	if h, ok := strings.CutPrefix(rest, "["); ok {
		if j := strings.Index(h, "]"); j >= 0 {
			if p, ok := strings.CutPrefix(h[j+1:], ":"); ok {
				return h[:j], p
			}
			return h[:j], ""
		}
	}
	if j := strings.LastIndex(rest, ":"); j >= 0 {
		return rest[:j], rest[j+1:]
	}
	return rest, ""
}

// clone is phase 5: the gateway is the box's only reachable remote, and its
// URL (with the repo already spliced in by the host) is the whole clone
// spec. Absence of the remote env means a gateless step: later phases run in
// a scratch directory and the signing phase is skipped.
func (b *Box) clone(ctx context.Context) error {
	if b.Env.RemoteURL == "" {
		dir, err := os.MkdirTemp("", "faber-box-work-*")
		if err != nil {
			return &boxError{Reason: contract.ReasonCloneFailed, Detail: fmt.Sprintf("scratch workdir: %v", err)}
		}
		b.Workdir = dir
		b.Log.InfoContext(ctx, "gateless step; scratch workdir", "dir", dir)
		return nil
	}
	target := filepath.Join(b.Env.WorkspaceDir, repoDirName(b.Env.RemoteURL))
	if err := os.MkdirAll(b.Env.WorkspaceDir, 0o755); err != nil {
		return &boxError{Reason: contract.ReasonCloneFailed, Detail: fmt.Sprintf("workspace dir: %v", err)}
	}
	argv := []string{"git", "clone"}
	if b.Env.GitCache != "" {
		// Borrow objects from the shared read-only cache; --if-able degrades to a
		// plain clone if the cache is unusable rather than failing the step.
		argv = append(argv, "--reference-if-able", b.Env.GitCache)
	}
	argv = append(argv, b.Env.RemoteURL, target)
	res, err := b.Runner.Run(ctx, CmdSpec{Argv: argv, Env: b.Environ})
	if err != nil {
		return &boxError{Reason: contract.ReasonCloneFailed, Detail: err.Error(), ExitCode: res.ExitCode}
	}
	if res.ExitCode != 0 {
		// The remote URL never enters the record: a userinfo-bearing URL
		// would persist its credential. The box has exactly one remote.
		return &boxError{
			Reason:     contract.ReasonCloneFailed,
			Detail:     fmt.Sprintf("git clone exited %d", res.ExitCode),
			ExitCode:   res.ExitCode,
			StderrTail: string(res.StderrTail),
		}
	}
	b.Workdir = target
	return nil
}

// repoDirName derives the checkout directory name from the clone URL.
func repoDirName(url string) string {
	name := strings.TrimSuffix(path.Base(strings.TrimSuffix(strings.ReplaceAll(url, "\\", "/"), "/")), ".git")
	if name == "" || name == "." || name == "/" {
		return "repo"
	}
	return name
}

// configureSigning is phase 6: the public key is read from the forwarded
// agent socket; exactly one key must be listed — zero or several is an
// identity-binding violation. The same key signs commits and authenticates
// SSH: one fingerprint, one role. What that fingerprint may do is the user's
// gate service's business, never the box's. Skipped on gateless steps.
func (b *Box) configureSigning(ctx context.Context) error {
	if b.Env.RemoteURL == "" {
		return nil
	}
	res, err := b.Runner.Run(ctx, CmdSpec{Argv: []string{"ssh-add", "-L"}, Env: b.Environ})
	if err != nil {
		return &boxError{Reason: contract.ReasonSigning, Detail: err.Error()}
	}
	if res.ExitCode != 0 {
		return &boxError{
			Reason:     contract.ReasonSigning,
			Detail:     fmt.Sprintf("ssh-add -L exited %d (no forwarded agent?)", res.ExitCode),
			ExitCode:   res.ExitCode,
			StderrTail: string(res.StderrTail),
		}
	}
	keys := nonEmptyLines(string(res.Stdout))
	if len(keys) != 1 {
		return &boxError{
			Reason: contract.ReasonSigning,
			Detail: fmt.Sprintf("forwarded agent lists %d keys; exactly one key per box", len(keys)),
		}
	}
	fields := strings.Fields(keys[0])
	if len(fields) < 2 {
		return &boxError{Reason: contract.ReasonSigning, Detail: "forwarded agent listed a malformed public key line"}
	}
	pub := fields[0] + " " + fields[1]

	name := b.Env.GitName
	if name == "" {
		name = "faber-" + b.Env.Identity
	}
	email := b.Env.GitEmail
	if email == "" {
		email = "faber-" + b.Env.Identity + "@box.invalid"
	}
	for _, kv := range [][2]string{
		{"gpg.format", "ssh"},
		{"user.signingkey", "key::" + pub},
		{"commit.gpgsign", "true"},
		{"user.name", name},
		{"user.email", email},
	} {
		res, err := b.Runner.Run(ctx, CmdSpec{
			Argv: []string{"git", "config", kv[0], kv[1]},
			Dir:  b.Workdir,
			Env:  b.Environ,
		})
		if err != nil {
			return &boxError{Reason: contract.ReasonSigning, Detail: err.Error()}
		}
		if res.ExitCode != 0 {
			return &boxError{
				Reason:     contract.ReasonSigning,
				Detail:     fmt.Sprintf("git config %s exited %d", kv[0], res.ExitCode),
				ExitCode:   res.ExitCode,
				StderrTail: string(res.StderrTail),
			}
		}
	}
	b.Log.InfoContext(ctx, "signing configured", "key", pub, "committer", name)
	return nil
}

// nonEmptyLines splits s into trimmed, non-empty lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}
