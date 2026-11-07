// Package box is the in-container half of the agent module: the phase
// sequencer the faber-box binary runs as every step container's entry
// program. It owns the fixed, engine-defined phase order — skills link, env
// contract, delegated secrets, host-key policy, gateway clone, signing config,
// context hook, prelude hook, agent invocation, postlude hook, result
// emission — with fail-stop between phases and one attempt record on every
// exit path. There is no in-container DAG and nothing here is configurable
// per template.
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
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
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

	// Frames is the box's standard output: the frame channel the events
	// stream and the result directory travel on. The binary wires os.Stdout;
	// nil ships nothing (unit tests with a fake runner).
	Frames *contract.FrameWriter

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

	// skipAgent records the prelude's honored skip request (the bundle
	// sidecar's SkipAgentKey on an agent-skippable template): the agent
	// phase becomes a logged no-op and the attempt record is marked.
	skipAgent bool
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
	// user marks a user-filled phase (hook or agent): the one class of phase
	// after which a halt request (the halt file) is honored.
	user bool
}

// phases is the engine-owned sequence — the spec's fixed box phase order is
// literally this slice. It is package data, not configuration: no template,
// env value, or flag reorders it.
var phases = []phase{
	{"skills", (*Box).linkSkills, false},
	{"env", (*Box).checkEnv, false},
	{"secrets", (*Box).loadSecrets, false},
	{"hostkey", (*Box).applyHostKeyPolicy, false},
	{"clone", (*Box).clone, false},
	{"signing", (*Box).configureSigning, false},
	{"context", (*Box).runContextHook, true},
	{"prelude", (*Box).runPreludeHook, true},
	{"agent", (*Box).runAgent, true},
	{"postlude", (*Box).runPostludeHook, true},
	{"result", (*Box).emitResult, false},
}

// Main drives the fixed order with fail-stop between phases: no phase ever
// runs after a failed one, and every failure converges on the handoff funnel.
// After each user-filled phase that exited 0, a halt request (halt.json in
// the result directory) is honored: the halted attempt record is written and
// no later phase runs — a prelude halt skips the agent, an agent halt skips
// the postlude. The check runs only on an orderly exit; a phase that fails
// after writing the halt file is a failure, never a halt. The return value
// is the process exit code (the shipped record, not the code, is what the
// host trusts); main ships the result directory after it returns.
func Main(ctx context.Context, b *Box) int {
	if err := b.enterRunUser(ctx); err != nil {
		b.failStop(ctx, "preamble", err)
		return 1
	}
	stopTail := b.tailEvents(ctx)
	defer stopTail()
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
		if p.user {
			halted, herr := b.checkHalt(ctx, p.name)
			if herr != nil {
				b.failStop(ctx, p.name, herr)
				return 1
			}
			if halted {
				return 0
			}
		}
	}
	return 0
}

// enterRunUser is the privileged preamble (arch phase 0). The container starts
// as root because the writable mounts arrive root-owned; this chowns exactly
// those mounts (the result tmpfs among them) to the run user, exports HOME,
// and drops privileges — setgroups to the run group, setgid, setuid — so
// every phase below, and the untrusted agent above all, runs non-root. (The
// agent socket needs no group: the relay creates it inside the container
// and chowns it to the run user.) The toolset store paths stay root-owned and read-only. A box
// already running non-root, or with no run uid (a gateless local invocation
// with no root to drop), is a no-op. It is the only moment faber-box holds
// privilege.
func (b *Box) enterRunUser(ctx context.Context) error {
	if os.Getuid() != 0 || b.Env.RunUID == 0 {
		return nil
	}
	fail := func(step string, err error) error {
		return &boxError{Reason: contract.ReasonEnvContract, Detail: fmt.Sprintf("preamble: %s: %v", step, err)}
	}
	for _, dir := range []string{b.Env.WorkspaceDir, b.Env.ResultDir, b.Env.BundleDir, "/tmp", contract.ContainerHome} {
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
	if err := ensureRunUser(etcDir, b.Env.RunUID, b.Env.RunGID); err != nil {
		return fail("run user", err)
	}
	b.setEnv("HOME", contract.ContainerHome)
	if err := dropTo(b.Env.RunUID, b.Env.RunGID); err != nil {
		return fail("drop", err)
	}
	b.Log.InfoContext(ctx, "dropped to run user", "uid", b.Env.RunUID, "gid", b.Env.RunGID)
	return nil
}

// etcDir is where the run user's passwd and group entries go; a variable so
// the tests can point it at a scratch directory.
var etcDir = "/etc"

// ensureRunUser gives the run user a passwd and a group entry. A toolset
// image carries no users (the uid is a run-time value and the image stays
// generic), but OpenSSH looks the calling uid up before it does anything —
// "No user exists for uid" — so the clone and the signing phase need the
// entry. Appends to the files, creating them when absent; a uid or gid
// already present is left alone (OpenSSH then takes `~` from that entry's
// home: pinned mode names its known-hosts file explicitly, TOFU mode warns
// and goes on). The shell field is unused by anything faber runs. Needs a
// writable /etc, which every image has today. Root only, like its callers.
func ensureRunUser(etc string, uid, gid int) error {
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return err
	}
	group := fmt.Sprintf("box:x:%d:\n", gid)
	if err := appendUnlessID(filepath.Join(etc, "group"), 2, strconv.Itoa(gid), group); err != nil {
		return err
	}
	passwd := fmt.Sprintf("box:x:%d:%d:faber box:%s:/bin/sh\n", uid, gid, contract.ContainerHome)
	return appendUnlessID(filepath.Join(etc, "passwd"), 2, strconv.Itoa(uid), passwd)
}

// appendUnlessID appends line to the colon-separated file at path unless a
// line already carries id in the given field.
func appendUnlessID(path string, field int, id, line string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, l := range strings.Split(string(existing), "\n") {
		if f := strings.Split(l, ":"); len(f) > field && f[field] == id {
			return nil
		}
	}
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		line = "\n" + line
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

// dropTo leaves root for uid:gid — setgroups to that one group, setgid,
// setuid (all threads, since Go 1.16) — the preamble's drop and the relay's.
func dropTo(uid, gid int) error {
	if err := syscall.Setgroups([]int{gid}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
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
// record plus a snapshot of the bundle directory into the result directory
// (shipped to the host with everything else, so container removal cannot
// lose them), then the failed attempt record carrying the handoff pointer.
// Errors here can only be logged — the host synthesizes a box-vanished
// record when no readable record remains.
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
		// A failure after an honored skip (a failing postlude, an unmet
		// output contract) still records that no agent ran.
		AgentSkipped: b.skipAgent,
		Check:        b.Env.Check,
		Timing:       b.Timing,
		Attempt:      b.Env.Attempt,
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
		"env":      contract.ReasonEnvContract,
		"secrets":  contract.ReasonSecrets,
		"hostkey":  contract.ReasonHostKeyPolicy,
		"clone":    contract.ReasonCloneFailed,
		"signing":  contract.ReasonSigning,
		"context":  contract.ReasonHookFailed,
		"prelude":  contract.ReasonHookFailed,
		"agent":    contract.ReasonAgentFailed,
		"postlude": contract.ReasonHookFailed,
		"result":   contract.ReasonResultWrite,
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
	if b.Env.AgentSocket != "" {
		// Before anything that could use the agent — the clone, or on a
		// gateless step the hooks — and before any untrusted code runs
		// beside the relay's own setup.
		if err := b.waitAgentSocket(ctx); err != nil {
			return err
		}
	}
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

// agentSocketWait bounds the clone phase's wait for the relay's socket: the
// host attaches the relay a moment after the container starts, so a socket
// still missing after this long means it never will come. A variable so the
// tests can lower it.
var agentSocketWait = 30 * time.Second

// waitAgentSocket waits until SSH_AUTH_SOCK accepts a connection from this
// process — the run user, after the preamble — so a slow attach is a short
// wait and a missing one a recorded signing failure rather than a hang or
// an opaque clone error. A dial, not a stat: the socket exists for an
// instant before the relay has given it to the run user.
func (b *Box) waitAgentSocket(ctx context.Context) error {
	deadline := time.Now().Add(agentSocketWait)
	for {
		if conn, err := net.Dial("unix", b.Env.AgentSocket); err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return &boxError{Reason: contract.ReasonSigning,
				Detail: fmt.Sprintf("agent socket %s did not appear within %s: the host's agent relay never attached", b.Env.AgentSocket, agentSocketWait)}
		}
		select {
		case <-ctx.Done():
			return &boxError{Reason: contract.ReasonSigning, Detail: "waiting for the agent socket: " + ctx.Err().Error()}
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// repoDirName derives the checkout directory name from the clone URL.
func repoDirName(url string) string {
	name := strings.TrimSuffix(path.Base(strings.TrimSuffix(strings.ReplaceAll(url, "\\", "/"), "/")), ".git")
	if name == "" || name == "." || name == "/" {
		return "repo"
	}
	return name
}

// configureSigning is phase 6: the public key is read from the box's agent
// socket (the host's proxy, through the relay); exactly one key must be listed — zero or several is an
// identity-binding violation. The same key signs commits and authenticates
// SSH: one fingerprint, one role. What that fingerprint may do is the user's
// gate service's business, never the box's. Skipped on gateless steps.
//
// A gated step requires an explicit committer email: a synthetic address
// would sign and pass a key-based gate, yet a forge that ties signature
// verification to a registered account email could never verify it — a
// silent misconfiguration this abort surfaces at the first run.
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
			Detail:     fmt.Sprintf("ssh-add -L exited %d (no agent behind the relay?)", res.ExitCode),
			ExitCode:   res.ExitCode,
			StderrTail: string(res.StderrTail),
		}
	}
	keys := nonEmptyLines(string(res.Stdout))
	if len(keys) != 1 {
		return &boxError{
			Reason: contract.ReasonSigning,
			Detail: fmt.Sprintf("the agent lists %d keys; exactly one key per box", len(keys)),
		}
	}
	fields := strings.Fields(keys[0])
	if len(fields) < 2 {
		return &boxError{Reason: contract.ReasonSigning, Detail: "the agent listed a malformed public key line"}
	}
	pub := fields[0] + " " + fields[1]

	if b.Env.GitEmail == "" {
		return &boxError{
			Reason: contract.ReasonSigning,
			Detail: "no committer email for this role; a gated step requires one — register it: faber add-key --role <role> --fingerprint <fp> --git-email <addr>",
		}
	}
	name := b.Env.GitName
	if name == "" {
		name = "faber-" + b.Env.Identity
	}
	for _, kv := range [][2]string{
		{"gpg.format", "ssh"},
		{"user.signingkey", "key::" + pub},
		{"commit.gpgsign", "true"},
		{"user.name", name},
		{"user.email", b.Env.GitEmail},
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
