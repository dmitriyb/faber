package box

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
)

// fakeRunner records every CmdSpec and answers from a scripted handler; unit
// tests never exec a process, docker, or a real agent CLI.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []CmdSpec
	streams []bool
	handle  func(spec CmdSpec, stream bool) (CmdResult, error)
}

func (f *fakeRunner) Run(ctx context.Context, spec CmdSpec) (CmdResult, error) {
	return f.record(spec, false)
}

func (f *fakeRunner) Stream(ctx context.Context, spec CmdSpec) (CmdResult, error) {
	return f.record(spec, true)
}

func (f *fakeRunner) record(spec CmdSpec, stream bool) (CmdResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.streams = append(f.streams, stream)
	f.mu.Unlock()
	if f.handle == nil {
		return CmdResult{}, nil
	}
	return f.handle(spec, stream)
}

// argvs renders the recorded calls as space-joined argv heads for order
// assertions.
func (f *fakeRunner) argvs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		head := c.Argv
		if len(head) > 2 {
			head = head[:2]
		}
		out = append(out, strings.Join(head, " "))
	}
	return out
}

// testDirs are the scratch stand-ins for the box mounts.
type testDirs struct {
	result, bundle, hooks, secrets, workspace string
}

func newTestDirs(t *testing.T) testDirs {
	t.Helper()
	root := t.TempDir()
	// In-process runs create scratch workdirs and known-hosts files via
	// os.MkdirTemp/os.CreateTemp; keep them under the test root.
	t.Setenv("TMPDIR", root)
	d := testDirs{
		result:    filepath.Join(root, "result"),
		bundle:    filepath.Join(root, "bundle"),
		hooks:     filepath.Join(root, "hooks"),
		secrets:   filepath.Join(root, "secrets"),
		workspace: filepath.Join(root, "workspace"),
	}
	for _, dir := range []string{d.result, d.bundle, d.hooks, d.secrets, d.workspace} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// baseEnv is a valid box env contract over the test dirs. Overrides with an
// empty value delete the key.
func baseEnv(d testDirs, overrides map[string]string) []string {
	env := map[string]string{
		contract.EnvSkill:        "skill-a",
		contract.EnvAgentCLI:     "agent-cli",
		contract.EnvIdentity:     "role-a",
		contract.EnvResultDir:    d.result,
		contract.EnvBundleDir:    d.bundle,
		contract.EnvHooksDir:     d.hooks,
		contract.EnvSecretsDir:   d.secrets,
		contract.EnvWorkspaceDir: d.workspace,
		contract.EnvAttempt:      "1",
	}
	for k, v := range overrides {
		if v == "" {
			delete(env, k)
			continue
		}
		env[k] = v
	}
	var out []string
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func newTestBox(t *testing.T, d testDirs, overrides map[string]string, fr *fakeRunner) *Box {
	t.Helper()
	environ := baseEnv(d, overrides)
	return New(ParseEnv(environ), fr, environ, nil)
}

// oneKeyHandler answers the deterministic setup calls of a repo-bound step:
// clone and config succeed, the forwarded agent lists exactly one key.
func oneKeyHandler(next func(spec CmdSpec, stream bool) (CmdResult, error)) func(CmdSpec, bool) (CmdResult, error) {
	return func(spec CmdSpec, stream bool) (CmdResult, error) {
		switch spec.Argv[0] {
		case "ssh-add":
			return CmdResult{Stdout: []byte("ssh-ed25519 AAAATESTKEY comment@box\n")}, nil
		case "git":
			return CmdResult{}, nil
		}
		if next != nil {
			return next(spec, stream)
		}
		return CmdResult{}, nil
	}
}

func writeHook(t *testing.T, d testDirs, name string) string {
	t.Helper()
	path := filepath.Join(d.hooks, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeOutput(t *testing.T, d testDirs, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d.result, contract.OutputFile), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readRecord(t *testing.T, d testDirs) contract.Result {
	t.Helper()
	rec, err := contract.ReadResultFile(d.result)
	if err != nil {
		t.Fatalf("read attempt record: %v", err)
	}
	return rec
}

func readHandoff(t *testing.T, d testDirs) contract.Handoff {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(d.result, contract.HandoffFile))
	if err != nil {
		t.Fatalf("read handoff: %v", err)
	}
	var h contract.Handoff
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatalf("parse handoff: %v", err)
	}
	return h
}

// loggedPhases extracts the "phase start" sequence from a captured JSON log.
func loggedPhases(t *testing.T, log *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("non-JSON log line %q: %v", line, err)
		}
		if rec["msg"] == "phase start" {
			out = append(out, rec["phase"].(string))
		}
	}
	return out
}

// Verifies 93ba0858d75f: every agent step executes the same engine-owned
// internal sequence — env, secrets, hostkey, clone, signing, context,
// prelude, agent, result — with no in-container DAG.
func TestFixedPhaseOrderHappyPath(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = oneKeyHandler(func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == filepath.Join(d.hooks, contract.HookPrelude) {
			// The prelude produces the bundle.
			if err := os.WriteFile(filepath.Join(d.bundle, contract.ContextDoc), []byte("body\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{}`)
		}
		return CmdResult{}, nil
	})
	writeHook(t, d, contract.HookContext)
	writeHook(t, d, contract.HookPrelude)

	var logBuf bytes.Buffer
	environ := baseEnv(d, map[string]string{"FABER_REMOTE_URL": "/gw/repo-a.git"})
	b := New(ParseEnv(environ), fr, environ, slog.New(slog.NewJSONHandler(&logBuf, nil)))
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	want := []string{"env", "secrets", "hostkey", "clone", "signing", "context", "prelude", "agent", "result"}
	got := loggedPhases(t, &logBuf)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("phase order = %v, want %v", got, want)
	}

	// The subprocess sequence mirrors the phases: clone, signing (ssh-add +
	// 5 git config), hooks, agent.
	wantCalls := []string{
		"git clone",
		"ssh-add -L",
		"git config", "git config", "git config", "git config", "git config",
		filepath.Join(d.hooks, contract.HookContext),
		filepath.Join(d.hooks, contract.HookPrelude),
		"agent-cli -p",
	}
	gotCalls := fr.argvs()
	if fmt.Sprint(gotCalls) != fmt.Sprint(wantCalls) {
		t.Fatalf("call order = %v, want %v", gotCalls, wantCalls)
	}
	if rec := readRecord(t, d); rec.Status != contract.StatusOK {
		t.Fatalf("record status = %q, want ok", rec.Status)
	}
}

// Verifies 93ba0858d75f: the env contract phase collects every violation and
// aborts with reason env-contract before anything else runs.
func TestEnvContractViolationsCollected(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	b := newTestBox(t, d, map[string]string{
		contract.EnvSkill:          "", // required, missing
		contract.EnvAgentCLI:       "",
		contract.EnvRequiredInputs: "alpha",
	}, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("no subprocess may run after an env-contract violation, got %v", fr.argvs())
	}
	h := readHandoff(t, d)
	if h.Phase != "env" || h.Reason != contract.ReasonEnvContract {
		t.Fatalf("handoff = %+v, want phase env reason env-contract", h)
	}
	rec := readRecord(t, d)
	for _, part := range []string{contract.EnvSkill, contract.EnvAgentCLI, "ALPHA"} {
		if !strings.Contains(rec.Error.Detail, part) {
			t.Errorf("detail %q does not name %q — violations must be collected", rec.Error.Detail, part)
		}
	}
}

// Verifies 93ba0858d75f: each file under the secrets dir is exported into
// the child environment under its uppercased basename — reachable by hooks,
// never present in the docker argv (which the box never assembles).
func TestSecretsExportedToChildEnv(t *testing.T) {
	d := newTestDirs(t)
	if err := os.WriteFile(filepath.Join(d.secrets, "service-token"), []byte("sekret-v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fr := &fakeRunner{}
	b := newTestBox(t, d, nil, fr)
	if err := b.checkEnv(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.loadSecrets(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "SERVICE_TOKEN=sekret-v"
	found := false
	for _, kv := range b.Environ {
		if kv == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("child env misses %q", want)
	}
}

// Verifies 93ba0858d75f: host-key policy is pinned (fail closed), explicit
// TOFU, or abort-before-network for an ssh remote; local-path remotes need
// no policy.
func TestHostKeyPolicy(t *testing.T) {
	tests := []struct {
		name       string
		overrides  map[string]string
		wantErr    bool
		wantEnvSub string
	}{
		{
			name:       "pinned key fails closed",
			overrides:  map[string]string{"FABER_REMOTE_URL": "ssh://git@gw/repo-a.git", "FABER_HOST_KEY": "ssh-ed25519 AAAAPIN gw-host"},
			wantEnvSub: "StrictHostKeyChecking=yes",
		},
		{
			name:       "tofu opt-in selects accept-new",
			overrides:  map[string]string{"FABER_REMOTE_URL": "ssh://git@gw/repo-a.git", "FABER_HOST_KEY_TOFU": "1"},
			wantEnvSub: "StrictHostKeyChecking=accept-new",
		},
		{
			name:      "ssh remote with no policy aborts before network",
			overrides: map[string]string{"FABER_REMOTE_URL": "ssh://git@gw/repo-a.git"},
			wantErr:   true,
		},
		{
			name:      "scp-like remote with no policy aborts",
			overrides: map[string]string{"FABER_REMOTE_URL": "git@gw:repo-a.git"},
			wantErr:   true,
		},
		{
			name:      "path remote needs no policy",
			overrides: map[string]string{"FABER_REMOTE_URL": "/gw/repo-a.git"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDirs(t)
			b := newTestBox(t, d, tt.overrides, &fakeRunner{})
			if err := b.checkEnv(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := b.applyHostKeyPolicy(context.Background())
			if tt.wantErr {
				berr := &boxError{}
				if err == nil || !asBoxErrorOK(err, &berr) || berr.Reason != contract.ReasonHostKeyPolicy {
					t.Fatalf("err = %v, want reason host-key-policy", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			gitSSH := envLookup(b.Environ, "GIT_SSH_COMMAND")
			if tt.wantEnvSub == "" {
				if gitSSH != "" {
					t.Fatalf("GIT_SSH_COMMAND unexpectedly set: %q", gitSSH)
				}
				return
			}
			if !strings.Contains(gitSSH, tt.wantEnvSub) {
				t.Fatalf("GIT_SSH_COMMAND = %q, want substring %q", gitSSH, tt.wantEnvSub)
			}
		})
	}
}

// Verifies 93ba0858d75f: a pinned bare key line lands in the known-hosts
// file under the pattern OpenSSH actually looks up — the bare host on the
// default port, "[host]:port" on a non-default one (a pin for ssh://gw:2222
// written as "gw ..." would never match and every clone would fail).
func TestPinnedKnownHostsContent(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "non-default port uses the bracketed pattern",
			url:  "ssh://git@gw.internal:2222/repo-a.git",
			want: "[gw.internal]:2222 ssh-ed25519 AAAAPIN gw-host\n",
		},
		{
			name: "default port uses the bare host",
			url:  "ssh://git@gw.internal/repo-a.git",
			want: "gw.internal ssh-ed25519 AAAAPIN gw-host\n",
		},
		{
			name: "explicit port 22 uses the bare host",
			url:  "ssh://git@gw.internal:22/repo-a.git",
			want: "gw.internal ssh-ed25519 AAAAPIN gw-host\n",
		},
		{
			name: "scp-like form has no port syntax",
			url:  "git@gw.internal:org/repo-a.git",
			want: "gw.internal ssh-ed25519 AAAAPIN gw-host\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDirs(t)
			b := newTestBox(t, d, map[string]string{
				"FABER_REMOTE_URL": tt.url,
				"FABER_HOST_KEY":   "ssh-ed25519 AAAAPIN gw-host",
			}, &fakeRunner{})
			if err := b.checkEnv(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := b.applyHostKeyPolicy(context.Background()); err != nil {
				t.Fatal(err)
			}
			gitSSH := envLookup(b.Environ, "GIT_SSH_COMMAND")
			_, after, ok := strings.Cut(gitSSH, "UserKnownHostsFile=")
			if !ok {
				t.Fatalf("GIT_SSH_COMMAND = %q, want UserKnownHostsFile", gitSSH)
			}
			file := strings.Fields(after)[0]
			t.Cleanup(func() { os.Remove(file) })
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tt.want {
				t.Fatalf("known-hosts = %q, want %q", raw, tt.want)
			}
		})
	}
}

// Verifies 93ba0858d75f: the derived pattern is exactly what ssh-keygen -F
// resolves, including IPv6 literals — the fail-closed pin must match.
func TestKnownHostsPatternMatchesOpenSSHLookup(t *testing.T) {
	if got := knownHostsPattern("ssh://git@gw.internal:2222/x.git"); got != "[gw.internal]:2222" {
		t.Fatalf("pattern = %q", got)
	}
	if got := knownHostsPattern("ssh://[::1]:2222/x.git"); got != "[::1]:2222" {
		t.Fatalf("ipv6 pattern = %q", got)
	}
	if got := knownHostsPattern("ssh://[::1]/x.git"); got != "::1" {
		t.Fatalf("ipv6 default-port pattern = %q", got)
	}
}

// Verifies 93ba0858d75f: the TOFU opt-in is exactly the contract value "1" —
// any other value is an env-contract violation, never a silent accept-new.
func TestTOFUStrictParse(t *testing.T) {
	for _, tt := range []struct {
		value    string
		wantTOFU bool
		wantErr  bool
	}{
		{"1", true, false},
		{"0", false, true},
		{"false", false, true},
		{"yes", false, true},
	} {
		t.Run("value "+tt.value, func(t *testing.T) {
			d := newTestDirs(t)
			b := newTestBox(t, d, map[string]string{
				"FABER_REMOTE_URL":    "ssh://git@gw/repo-a.git",
				"FABER_HOST_KEY_TOFU": tt.value,
			}, &fakeRunner{})
			err := b.checkEnv(context.Background())
			if tt.wantErr {
				berr := &boxError{}
				if err == nil || !asBoxErrorOK(err, &berr) || berr.Reason != contract.ReasonEnvContract {
					t.Fatalf("err = %v, want env-contract violation", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b.Env.TOFU != tt.wantTOFU {
				t.Fatalf("TOFU = %v, want %v", b.Env.TOFU, tt.wantTOFU)
			}
		})
	}
}

// Verifies 93ba0858d75f: a clone failure's persisted records never carry the
// remote URL — a userinfo-bearing URL would land its credential in the
// detail.
func TestCloneFailureRedactsRemoteURL(t *testing.T) {
	const token = "sekret-token-98765"
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "git" && spec.Argv[1] == "clone" {
			return CmdResult{ExitCode: 128, StderrTail: []byte("fatal: authentication failed\n")}, nil
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, map[string]string{
		"FABER_REMOTE_URL": "https://user:" + token + "@gw.invalid/repo-a.git",
	}, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatal("want failure exit")
	}
	for _, name := range []string{contract.ResultFile, contract.HandoffFile} {
		raw, err := os.ReadFile(filepath.Join(d.result, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), token) {
			t.Fatalf("%s leaks the remote URL credential", name)
		}
	}
	rec := readRecord(t, d)
	if rec.Error.Reason != contract.ReasonCloneFailed || !strings.Contains(rec.Error.Detail, "exited 128") {
		t.Fatalf("record error = %+v", rec.Error)
	}
}

// Verifies 93ba0858d75f: absence of the remote env means a gateless step —
// clone and signing are skipped, later phases run in a scratch directory.
func TestGatelessStepSkipsCloneAndSigning(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{}`)
		}
		return CmdResult{}, nil
	}
	var logBuf bytes.Buffer
	environ := baseEnv(d, nil)
	b := New(ParseEnv(environ), fr, environ, slog.New(slog.NewJSONHandler(&logBuf, nil)))
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	t.Cleanup(func() { os.RemoveAll(b.Workdir) })
	for _, call := range fr.argvs() {
		if strings.HasPrefix(call, "git") || strings.HasPrefix(call, "ssh-add") {
			t.Fatalf("gateless step ran %q", call)
		}
	}
	if b.Workdir == "" {
		t.Fatal("no scratch workdir was created")
	}
	// The agent ran in the scratch cwd.
	last := fr.calls[len(fr.calls)-1]
	if last.Dir != b.Workdir {
		t.Fatalf("agent cwd = %q, want scratch %q", last.Dir, b.Workdir)
	}
}

// Verifies 93ba0858d75f: signing config is derived from the forwarded agent
// socket; zero or several listed keys is an identity-binding violation.
func TestSigningOneKeyInvariant(t *testing.T) {
	for _, tt := range []struct {
		name string
		keys string
		want string // "" = ok
	}{
		{"one key configures signing", "ssh-ed25519 AAAA k1\n", ""},
		{"two keys abort", "ssh-ed25519 AAAA k1\nssh-ed25519 BBBB k2\n", "2 keys"},
		{"zero keys abort", "\n", "0 keys"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDirs(t)
			fr := &fakeRunner{}
			fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
				if spec.Argv[0] == "ssh-add" {
					return CmdResult{Stdout: []byte(tt.keys)}, nil
				}
				return CmdResult{}, nil
			}
			b := newTestBox(t, d, map[string]string{"FABER_REMOTE_URL": "/gw/repo-a.git"}, fr)
			if err := b.checkEnv(context.Background()); err != nil {
				t.Fatal(err)
			}
			err := b.configureSigning(context.Background())
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				var sawKey bool
				for _, c := range fr.calls {
					if len(c.Argv) == 4 && c.Argv[1] == "config" && c.Argv[2] == "user.signingkey" {
						sawKey = true
						if c.Argv[3] != "key::ssh-ed25519 AAAA" {
							t.Fatalf("signingkey = %q", c.Argv[3])
						}
					}
				}
				if !sawKey {
					t.Fatal("git config user.signingkey never ran")
				}
				return
			}
			berr := &boxError{}
			if err == nil || !asBoxErrorOK(err, &berr) || berr.Reason != contract.ReasonSigning || !strings.Contains(berr.Detail, tt.want) {
				t.Fatalf("err = %v, want signing violation naming %q", err, tt.want)
			}
		})
	}
}

// Verifies 93ba0858d75f: committer identity defaults derive from the box
// identity — faber-<identity> / faber-<identity>@box.invalid.
func TestSigningCommitterDefaults(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = oneKeyHandler(nil)
	b := newTestBox(t, d, map[string]string{"FABER_REMOTE_URL": "/gw/repo-a.git"}, fr)
	if err := b.checkEnv(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.configureSigning(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range fr.calls {
		if len(c.Argv) == 4 && c.Argv[1] == "config" {
			got[c.Argv[2]] = c.Argv[3]
		}
	}
	if got["user.name"] != "faber-role-a" || got["user.email"] != "faber-role-a@box.invalid" {
		t.Fatalf("committer = %q / %q, want faber-role-a defaults", got["user.name"], got["user.email"])
	}
	if got["gpg.format"] != "ssh" || got["commit.gpgsign"] != "true" {
		t.Fatalf("signing config = %v", got)
	}
}

// Verifies 93ba0858d75f: a failed phase converges on the fail-stop funnel —
// handoff record plus failed attempt record, nonzero exit, no later phase.
func TestFailStopWritesHandoffAndRecord(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "git" && spec.Argv[1] == "clone" {
			return CmdResult{ExitCode: 128, StderrTail: []byte("fatal: no route to gateway\n")}, nil
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, map[string]string{
		"FABER_REMOTE_URL":  "/gw/repo-a.git",
		"FABER_INPUT_ALPHA": "v1",
	}, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	h := readHandoff(t, d)
	if h.Phase != "clone" || h.Reason != contract.ReasonCloneFailed || h.ExitCode != 128 {
		t.Fatalf("handoff = %+v", h)
	}
	if !strings.Contains(h.StderrTail, "no route to gateway") {
		t.Fatalf("handoff stderr tail = %q", h.StderrTail)
	}
	if h.Inputs["ALPHA"] != "v1" {
		t.Fatalf("handoff inputs = %v, want the FABER_INPUT_* map", h.Inputs)
	}
	rec := readRecord(t, d)
	if rec.Status != contract.StatusFailed || rec.Error.Reason != contract.ReasonCloneFailed {
		t.Fatalf("record = %+v", rec)
	}
	if rec.Error.Handoff != contract.HandoffFile {
		t.Fatalf("record handoff pointer = %q, want %q", rec.Error.Handoff, contract.HandoffFile)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("phases after the failed clone ran: %v", fr.argvs())
	}
}

// envLookup finds key in a KEY=VALUE list.
func envLookup(environ []string, key string) string {
	for _, kv := range environ {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// asBoxErrorOK adapts errors.As for the table tests above.
func asBoxErrorOK(err error, target **boxError) bool {
	be, ok := err.(*boxError)
	if ok {
		*target = be
	}
	return ok
}
