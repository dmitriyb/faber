// Box lifecycle tests: integration scenarios spanning the whole sequencer —
// one box from env contract to attempt record. They run the real faber-box
// binary as a plain process, no docker: scratch directories stand in for the
// mounts and the env contract is set directly. A stub agent CLI records its
// argv and environment; a local bare git repository stands in for the
// gateway; a throwaway ssh-agent provides the forwarded identity.
package box_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/agent"
	"github.com/dmitriyb/faber/agent/contract"
	"github.com/dmitriyb/faber/config"
)

// boxBin is the compiled faber-box binary, built once in TestMain.
var boxBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "faber-box-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	boxBin = filepath.Join(dir, "faber-box")
	out, err := exec.Command("go", "build", "-o", boxBin, "github.com/dmitriyb/faber/cmd/faber-box").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build faber-box: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fixture is one boxed step attempt run as a plain process.
type fixture struct {
	t    *testing.T
	root string
	env  map[string]string

	resultDir, bundleDir, hooksDir, secretsDir, workspace string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		t:          t,
		root:       root,
		resultDir:  filepath.Join(root, "result"),
		bundleDir:  filepath.Join(root, "bundle"),
		hooksDir:   filepath.Join(root, "hooks"),
		secretsDir: filepath.Join(root, "secrets"),
		workspace:  filepath.Join(root, "workspace"),
	}
	for _, dir := range []string{f.resultDir, f.bundleDir, f.hooksDir, f.secretsDir, f.workspace} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stub := f.writeScript("agent-stub", agentStubScript)
	f.env = map[string]string{
		"PATH":                 os.Getenv("PATH"),
		"HOME":                 root,
		"TMPDIR":               root,
		"GIT_CONFIG_NOSYSTEM":  "1",
		"STUB_DIR":             root,
		contract.EnvSkill:      "skill-a",
		contract.EnvAgentCLI:   stub,
		contract.EnvIdentity:   "role-a",
		contract.EnvResultDir:  f.resultDir,
		contract.EnvBundleDir:  f.bundleDir,
		contract.EnvHooksDir:   f.hooksDir,
		contract.EnvSecretsDir: f.secretsDir,
		contract.EnvAttempt:    "3",
		"FABER_WORKSPACE_DIR":  f.workspace,
	}
	return f
}

// agentStubScript records its argv (one numbered file per argument) and its
// environment, optionally writes output.json and pushes a branch, then exits
// with a configurable code.
const agentStubScript = `#!/bin/sh
n=0
rm -f "$STUB_DIR"/argv.*
for a in "$@"; do
  n=$((n+1))
  printf '%s' "$a" > "$STUB_DIR/argv.$n"
done
env > "$STUB_DIR/agent-env"
if [ -n "$STUB_OUTPUT" ]; then
  printf '%s' "$STUB_OUTPUT" > "$FABER_RESULT_DIR/output.json"
fi
if [ -n "$STUB_PUSH_BRANCH" ]; then
  git push --quiet origin "HEAD:refs/heads/$STUB_PUSH_BRANCH"
fi
exit "${STUB_EXIT:-0}"
`

func (f *fixture) writeScript(name, content string) string {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func (f *fixture) writeHook(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.hooksDir, name), []byte(content), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

// happyHooks installs the reference pair: context writes CONTEXT.md, prelude
// appends bundle.env with BRANCH=t-1 and touches a marker.
func (f *fixture) happyHooks() {
	f.writeHook(contract.HookContext, "#!/bin/sh\nprintf 'ctx body line\\n' > \"$FABER_BUNDLE_DIR/CONTEXT.md\"\n")
	f.writeHook(contract.HookPrelude, "#!/bin/sh\nprintf 'BRANCH=t-1\\n' >> \"$FABER_BUNDLE_DIR/bundle.env\"\ntouch \"$STUB_DIR/prelude-marker\"\n")
}

// gateway seeds a local bare repository standing in for the gateway (path
// remote, no ssh transport) and points the box at it.
func (f *fixture) gateway() string {
	f.t.Helper()
	bare := filepath.Join(f.root, "gw", "repo-a.git")
	seed := filepath.Join(f.root, "seed")
	f.git("", "init", "-q", "--bare", bare)
	f.git(bare, "symbolic-ref", "HEAD", "refs/heads/main")
	f.git("", "init", "-q", seed)
	f.git(seed, "-c", "user.name=t", "-c", "user.email=t@t.invalid", "commit", "-q", "--allow-empty", "-m", "init")
	f.git(seed, "push", "-q", bare, "HEAD:refs/heads/main")
	f.env["FABER_REMOTE_URL"] = bare
	return bare
}

func (f *fixture) git(dir string, args ...string) {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+f.root, "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// sshAgent starts a throwaway agent loaded with n ephemeral ed25519 keys and
// wires SSH_AUTH_SOCK into the box env. It returns the public key lines.
func (f *fixture) sshAgent(n int) []string {
	f.t.Helper()
	dir, err := os.MkdirTemp("", "faber-ag") // short path: unix socket limit
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	agentCmd := exec.Command("ssh-agent", "-D", "-a", sock)
	if err := agentCmd.Start(); err != nil {
		f.t.Skipf("ssh-agent unavailable: %v", err)
	}
	f.t.Cleanup(func() { agentCmd.Process.Kill(); agentCmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			f.t.Fatal("ssh-agent socket never appeared")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var pubs []string
	for i := 0; i < n; i++ {
		key := filepath.Join(dir, fmt.Sprintf("k%d", i))
		if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", fmt.Sprintf("k%d", i), "-f", key).CombinedOutput(); err != nil {
			f.t.Fatalf("ssh-keygen: %v\n%s", err, out)
		}
		add := exec.Command("ssh-add", "-q", key)
		add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
		if out, err := add.CombinedOutput(); err != nil {
			f.t.Fatalf("ssh-add: %v\n%s", err, out)
		}
		raw, err := os.ReadFile(key + ".pub")
		if err != nil {
			f.t.Fatal(err)
		}
		pubs = append(pubs, strings.TrimSpace(string(raw)))
	}
	f.env["SSH_AUTH_SOCK"] = sock
	return pubs
}

// run executes the box binary with the fixture env and returns the exit code
// plus captured stderr (the structured log).
func (f *fixture) run() (int, string) {
	f.t.Helper()
	cmd := exec.Command(boxBin)
	keys := make([]string, 0, len(f.env))
	for k := range f.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmd.Env = append(cmd.Env, k+"="+f.env[k])
	}
	var stderr, stdout strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	err := cmd.Run()
	code := 0
	if err != nil {
		xerr, ok := err.(*exec.ExitError)
		if !ok {
			f.t.Fatalf("run faber-box: %v\nstderr:\n%s", err, stderr.String())
		}
		code = xerr.ExitCode()
	}
	return code, stderr.String()
}

func (f *fixture) record() contract.Result {
	f.t.Helper()
	rec, err := contract.ReadResultFile(f.resultDir)
	if err != nil {
		f.t.Fatalf("read record: %v", err)
	}
	return rec
}

func (f *fixture) handoff() contract.Handoff {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.resultDir, contract.HandoffFile))
	if err != nil {
		f.t.Fatalf("read handoff: %v", err)
	}
	var h contract.Handoff
	if err := json.Unmarshal(raw, &h); err != nil {
		f.t.Fatal(err)
	}
	return h
}

// stubArgv reads the argv the stub agent recorded; nil means it never ran.
func (f *fixture) stubArgv() []string {
	f.t.Helper()
	var argv []string
	for i := 1; ; i++ {
		raw, err := os.ReadFile(filepath.Join(f.root, fmt.Sprintf("argv.%d", i)))
		if err != nil {
			break
		}
		argv = append(argv, string(raw))
	}
	return argv
}

func phasesFromLog(t *testing.T, log string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, `"phase start"`) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec["phase"].(string))
	}
	return out
}

// Scenario 1. Verifies 93ba0858d75f, ae434449cac9 and ff8e85704b0a: the full
// run drives exactly skills/env/secrets/hostkey/clone/signing/context/prelude/
// agent/result (skills is a no-op when the fixture declares no skills leg); the
// stub's recorded prompt is /<skill> + blank line + the
// exact CONTEXT.md bytes; result.json is ok with the validated payload and
// attempt echoing FABER_ATTEMPT.
func TestLifecycle_HappyPathFixedOrder(t *testing.T) {
	f := newFixture(t)
	f.gateway()
	f.sshAgent(1)
	f.happyHooks()
	f.env[contract.EnvOutputSchema] = `{"verdict": {"type": "string", "enum": ["ok", "changes"], "required": true}}`
	f.env["STUB_OUTPUT"] = `{"verdict": "ok"}`
	f.env["STUB_PUSH_BRANCH"] = "t-1" // satisfy the declared side-effect

	code, log := f.run()
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, log)
	}
	want := []string{"skills", "env", "secrets", "hostkey", "clone", "signing", "context", "prelude", "agent", "result"}
	if got := phasesFromLog(t, log); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("phase order = %v, want %v", got, want)
	}
	argv := f.stubArgv()
	if len(argv) < 4 {
		t.Fatalf("stub argv = %q", argv)
	}
	if argv[0] != "-p" || argv[1] != "/skill-a\n\nctx body line\n" {
		t.Fatalf("prompt = %q, want /<skill> + blank line + exact CONTEXT.md bytes", argv[1])
	}
	if argv[2] != "--permission-mode" || argv[3] != "bypassPermissions" {
		t.Fatalf("argv = %q, want permission bypass", argv)
	}
	rec := f.record()
	if rec.Status != contract.StatusOK || rec.Payload["verdict"] != "ok" || rec.Attempt != 3 {
		t.Fatalf("record = %+v", rec)
	}
}

// Scenario 2. Verifies b880aa49b3b9: a failing prelude aborts before the
// agent — no argv recording exists, handoff carries phase/exit/stderr tail,
// the failed record points at it, and the process exits nonzero.
func TestLifecycle_PreludeFailureAbortsBeforeAgent(t *testing.T) {
	f := newFixture(t)
	f.writeHook(contract.HookContext, "#!/bin/sh\nprintf 'body\\n' > \"$FABER_BUNDLE_DIR/CONTEXT.md\"\n")
	f.writeHook(contract.HookPrelude, "#!/bin/sh\necho 'claim rejected' >&2\nexit 3\n")

	code, _ := f.run()
	if code == 0 {
		t.Fatal("want nonzero exit")
	}
	if argv := f.stubArgv(); argv != nil {
		t.Fatalf("the agent ran after a failed prelude: %q", argv)
	}
	h := f.handoff()
	if h.Phase != "prelude" || h.ExitCode != 3 || !strings.Contains(h.StderrTail, "claim rejected") {
		t.Fatalf("handoff = %+v", h)
	}
	rec := f.record()
	if rec.Status != contract.StatusFailed || rec.Error.Handoff != contract.HandoffFile {
		t.Fatalf("record = %+v", rec)
	}
}

// Scenario 3. Verifies b880aa49b3b9: hooks that exit 0 without a bundle
// abort with reason bundle-missing; the agent never starts.
func TestLifecycle_BundleLessPrelude(t *testing.T) {
	f := newFixture(t)
	f.writeHook(contract.HookContext, "#!/bin/sh\nexit 0\n")
	f.writeHook(contract.HookPrelude, "#!/bin/sh\nexit 0\n")

	code, _ := f.run()
	if code == 0 {
		t.Fatal("want nonzero exit")
	}
	if f.stubArgv() != nil {
		t.Fatal("the agent ran without a bundle")
	}
	h := f.handoff()
	if h.Phase != "prelude" || h.Reason != contract.ReasonBundleMissing {
		t.Fatalf("handoff = %+v, want bundle-missing", h)
	}
}

// Scenario 4. Verifies b880aa49b3b9 and db9815889696 (first pass): a
// hook-less template gets a synthesized CONTEXT.md enumerating the typed
// inputs, and the run completes.
func TestLifecycle_HookLessTemplate(t *testing.T) {
	f := newFixture(t)
	f.env["FABER_INPUT_ALPHA"] = "v1"
	f.env["FABER_INPUT_BETA"] = "v2"

	code, log := f.run()
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, log)
	}
	argv := f.stubArgv()
	if len(argv) < 2 {
		t.Fatalf("stub argv = %q", argv)
	}
	for _, want := range []string{"ALPHA: v1", "BETA: v2"} {
		if !strings.Contains(argv[1], want) {
			t.Errorf("synthesized prompt misses %q:\n%s", want, argv[1])
		}
	}
	if rec := f.record(); rec.Status != contract.StatusOK || !rec.Fallback {
		t.Fatalf("record = %+v, want ok fallback (stub wrote no output)", rec)
	}
}

// Scenario 5. Verifies 93ba0858d75f: signing config is derived from the
// forwarded agent — the clone's git config matches ssh-add -L — and two
// loaded keys abort at the signing phase naming the count.
func TestLifecycle_SigningDerivedFromForwardedAgent(t *testing.T) {
	t.Run("one key configures the clone", func(t *testing.T) {
		f := newFixture(t)
		f.gateway()
		pubs := f.sshAgent(1)
		f.env["STUB_OUTPUT"] = `{}`

		code, log := f.run()
		if code != 0 {
			t.Fatalf("exit = %d\n%s", code, log)
		}
		clone := filepath.Join(f.workspace, "repo-a")
		cfg := func(key string) string {
			out, err := exec.Command("git", "-C", clone, "config", "--local", key).Output()
			if err != nil {
				t.Fatalf("git config %s: %v", key, err)
			}
			return strings.TrimSpace(string(out))
		}
		if cfg("gpg.format") != "ssh" || cfg("commit.gpgsign") != "true" {
			t.Fatal("signing config missing in the clone")
		}
		fields := strings.Fields(pubs[0])
		if want := "key::" + fields[0] + " " + fields[1]; cfg("user.signingkey") != want {
			t.Fatalf("signingkey = %q, want %q", cfg("user.signingkey"), want)
		}
	})
	t.Run("two keys abort at the signing phase", func(t *testing.T) {
		f := newFixture(t)
		f.gateway()
		f.sshAgent(2)

		code, _ := f.run()
		if code == 0 {
			t.Fatal("want nonzero exit")
		}
		h := f.handoff()
		if h.Phase != "signing" || h.Reason != contract.ReasonSigning {
			t.Fatalf("handoff = %+v", h)
		}
		if rec := f.record(); !strings.Contains(rec.Error.Detail, "2 keys") {
			t.Fatalf("detail = %q, want the key count named", rec.Error.Detail)
		}
	})
}

// Scenario 6. Verifies 93ba0858d75f: host-key policy precedes any network
// use — an ssh remote with neither pinned key nor TOFU aborts at the hostkey
// phase before clone; a pinned key lands in a known-hosts file and
// StrictHostKeyChecking=yes reaches git.
func TestLifecycle_HostKeyPolicy(t *testing.T) {
	t.Run("no policy aborts before clone", func(t *testing.T) {
		f := newFixture(t)
		f.env["FABER_REMOTE_URL"] = "ssh://git@gw.invalid/repo-a.git"

		code, log := f.run()
		if code == 0 {
			t.Fatal("want nonzero exit")
		}
		h := f.handoff()
		if h.Phase != "hostkey" || h.Reason != contract.ReasonHostKeyPolicy {
			t.Fatalf("handoff = %+v", h)
		}
		phases := phasesFromLog(t, log)
		if fmt.Sprint(phases) != fmt.Sprint([]string{"skills", "env", "secrets", "hostkey"}) {
			t.Fatalf("phases = %v, want to stop at hostkey", phases)
		}
	})
	t.Run("pinned key reaches git fail-closed", func(t *testing.T) {
		f := newFixture(t)
		// A PATH-stubbed git observes the clone env without any network: it
		// dumps GIT_SSH_COMMAND, copies the known-hosts file, and fakes the
		// clone directory; later git verbs just exit 0.
		stubDir := filepath.Join(f.root, "stubbin")
		if err := os.MkdirAll(stubDir, 0o755); err != nil {
			t.Fatal(err)
		}
		gitStub := `#!/bin/sh
if [ "$1" = "clone" ]; then
  printf '%s' "$GIT_SSH_COMMAND" > "$STUB_DIR/git-ssh-command"
  kh=$(printf '%s' "$GIT_SSH_COMMAND" | sed -n 's/.*UserKnownHostsFile=\([^ ]*\).*/\1/p')
  [ -n "$kh" ] && cp "$kh" "$STUB_DIR/known-hosts"
  mkdir -p "$3"
fi
exit 0
`
		if err := os.WriteFile(filepath.Join(stubDir, "git"), []byte(gitStub), 0o755); err != nil {
			t.Fatal(err)
		}
		f.sshAgent(1)
		f.env["PATH"] = stubDir + string(os.PathListSeparator) + f.env["PATH"]
		f.env["FABER_REMOTE_URL"] = "ssh://git@gw.invalid/repo-a.git"
		f.env["FABER_HOST_KEY"] = "ssh-ed25519 AAAAPINNEDKEY gw"
		f.env["STUB_OUTPUT"] = `{}`

		code, log := f.run()
		if code != 0 {
			t.Fatalf("exit = %d\n%s", code, log)
		}
		sshCmd, err := os.ReadFile(filepath.Join(f.root, "git-ssh-command"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(sshCmd), "StrictHostKeyChecking=yes") {
			t.Fatalf("GIT_SSH_COMMAND = %q", sshCmd)
		}
		kh, err := os.ReadFile(filepath.Join(f.root, "known-hosts"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(kh), "gw.invalid ssh-ed25519 AAAAPINNEDKEY gw") {
			t.Fatalf("known-hosts = %q, want the pinned line", kh)
		}
	})
}

// Scenario 7. Verifies 93ba0858d75f and b880aa49b3b9: delegated secret files
// reach hooks as environment, and a later failure's handoff carries the
// FABER_INPUT_* map but no trace of the secret value.
func TestLifecycle_SecretsReachHooksNeverHandoff(t *testing.T) {
	f := newFixture(t)
	const secret = "sekret-value-12345"
	if err := os.WriteFile(filepath.Join(f.secretsDir, "service_token"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.writeHook(contract.HookContext, "#!/bin/sh\nenv > \"$STUB_DIR/hook-env\"\nprintf 'body\\n' > \"$FABER_BUNDLE_DIR/CONTEXT.md\"\n")
	f.env["FABER_INPUT_ALPHA"] = "v1"
	f.env["STUB_EXIT"] = "17" // force a later failure

	code, _ := f.run()
	if code == 0 {
		t.Fatal("want nonzero exit")
	}
	hookEnv, err := os.ReadFile(filepath.Join(f.root, "hook-env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hookEnv), "SERVICE_TOKEN="+secret) {
		t.Fatal("the hook environment misses the delegated secret")
	}
	h := f.handoff()
	if h.Inputs["ALPHA"] != "v1" {
		t.Fatalf("handoff inputs = %v", h.Inputs)
	}
	for _, file := range []string{contract.HandoffFile, contract.ResultFile} {
		raw, err := os.ReadFile(filepath.Join(f.resultDir, file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s leaks the secret value", file)
		}
	}
}

// Scenario 8. Verifies ff8e85704b0a: an agent that exits 0 writing no
// output.json yields the fallback record — ok and empty under an
// all-optional schema, missing-output when a field is required.
func TestLifecycle_FallbackRecord(t *testing.T) {
	t.Run("all optional", func(t *testing.T) {
		f := newFixture(t)
		f.env[contract.EnvOutputSchema] = `{"note": {"type": "string"}}`
		code, _ := f.run()
		rec := f.record()
		if code != 0 || rec.Status != contract.StatusOK || !rec.Fallback || len(rec.Payload) != 0 {
			t.Fatalf("code=%d record=%+v", code, rec)
		}
	})
	t.Run("required field flips it to missing-output", func(t *testing.T) {
		f := newFixture(t)
		f.env[contract.EnvOutputSchema] = `{"note": {"type": "string", "required": true}}`
		code, _ := f.run()
		rec := f.record()
		if code == 0 || rec.Error == nil || rec.Error.Reason != contract.ReasonMissingOutput {
			t.Fatalf("code=%d record=%+v", code, rec)
		}
	})
}

// Scenario 9. Verifies ff8e85704b0a and f1ce19e94daa (first pass): schema
// violations are collected under reason output-schema; an extra undeclared
// field alone does not fail but is marked unthreaded.
func TestLifecycle_SchemaViolationsCollected(t *testing.T) {
	schema := `{"count": {"type": "int"}, "verdict": {"type": "string", "enum": ["ok", "changes"]}}`
	t.Run("two violations listed", func(t *testing.T) {
		f := newFixture(t)
		f.env[contract.EnvOutputSchema] = schema
		f.env["STUB_OUTPUT"] = `{"count": "nope", "verdict": "maybe"}`
		code, _ := f.run()
		rec := f.record()
		if code == 0 || rec.Error == nil || rec.Error.Reason != contract.ReasonOutputSchema {
			t.Fatalf("code=%d record=%+v", code, rec)
		}
		for _, part := range []string{"count", "verdict"} {
			if !strings.Contains(rec.Error.Detail, part) {
				t.Errorf("detail %q misses %q", rec.Error.Detail, part)
			}
		}
	})
	t.Run("extra field is unthreaded, not failed", func(t *testing.T) {
		f := newFixture(t)
		f.env[contract.EnvOutputSchema] = schema
		f.env["STUB_OUTPUT"] = `{"verdict": "ok", "surplus": true}`
		code, _ := f.run()
		rec := f.record()
		if code != 0 || rec.Status != contract.StatusOK {
			t.Fatalf("code=%d record=%+v", code, rec)
		}
		if fmt.Sprint(rec.Unthreaded) != "[surplus]" || rec.Payload["surplus"] != true {
			t.Fatalf("record = %+v", rec)
		}
	})
}

// Scenario 10. Verifies ff8e85704b0a: a valid-but-unfavorable payload is an
// ok result.
func TestLifecycle_UnfavorableIsNotFailure(t *testing.T) {
	f := newFixture(t)
	f.env[contract.EnvOutputSchema] = `{"verdict": {"type": "string", "enum": ["ok", "changes"], "required": true}}`
	f.env["STUB_OUTPUT"] = `{"verdict": "changes"}`
	code, _ := f.run()
	rec := f.record()
	if code != 0 || rec.Status != contract.StatusOK || rec.Payload["verdict"] != "changes" {
		t.Fatalf("code=%d record=%+v", code, rec)
	}
}

// Scenario 11. Verifies ff8e85704b0a: the declared BRANCH side-effect is
// verified against the gateway — pushed means ok, unpushed means failed with
// side-effect-unverified despite a schema-valid payload.
func TestLifecycle_DeclaredSideEffectVerified(t *testing.T) {
	setup := func(t *testing.T, push bool) *fixture {
		f := newFixture(t)
		f.gateway()
		f.sshAgent(1)
		f.happyHooks()
		f.env["STUB_OUTPUT"] = `{}`
		if push {
			f.env["STUB_PUSH_BRANCH"] = "t-1"
		}
		return f
	}
	t.Run("pushed branch verifies", func(t *testing.T) {
		f := setup(t, true)
		code, log := f.run()
		if code != 0 {
			t.Fatalf("exit = %d\n%s", code, log)
		}
		if rec := f.record(); rec.Status != contract.StatusOK {
			t.Fatalf("record = %+v", rec)
		}
	})
	t.Run("unpushed branch fails the attempt", func(t *testing.T) {
		f := setup(t, false)
		code, _ := f.run()
		rec := f.record()
		if code == 0 || rec.Error == nil || rec.Error.Reason != contract.ReasonSideEffectUnverified {
			t.Fatalf("code=%d record=%+v", code, rec)
		}
	})
}

// Scenario 12. Verifies ae434449cac9: an agent crash yields handoff
// {phase: agent, exit_code: 17} and a failed record; the stale output.json
// is never extracted.
func TestLifecycle_AgentCrash(t *testing.T) {
	f := newFixture(t)
	f.env["STUB_OUTPUT"] = `{"stale": true}`
	f.env["STUB_EXIT"] = "17"

	code, _ := f.run()
	if code == 0 {
		t.Fatal("want nonzero exit")
	}
	h := f.handoff()
	if h.Phase != "agent" || h.ExitCode != 17 || h.Reason != contract.ReasonAgentFailed {
		t.Fatalf("handoff = %+v", h)
	}
	rec := f.record()
	if rec.Status != contract.StatusFailed || rec.Payload != nil {
		t.Fatalf("record = %+v", rec)
	}
}

// Scenario 13. Verifies ff8e85704b0a: the host boundary returns the same
// record over a good directory, synthesizes box-vanished over a truncated
// one, and refuses to thread a hand-edited payload that breaks the schema.
func TestLifecycle_HostBoundary(t *testing.T) {
	schema := agent.OutputSchema{"verdict": config.FieldDef{Type: "string", Required: true, Enum: []string{"ok", "changes"}}}
	f := newFixture(t)
	f.env[contract.EnvOutputSchema] = `{"verdict": {"type": "string", "enum": ["ok", "changes"], "required": true}}`
	f.env["STUB_OUTPUT"] = `{"verdict": "ok"}`
	if code, log := f.run(); code != 0 {
		t.Fatalf("exit = %d\n%s", code, log)
	}

	rec, err := agent.ExtractResult(f.resultDir, schema)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != agent.StatusOK || rec.Payload["verdict"] != "ok" || rec.Attempt != 3 {
		t.Fatalf("extract = %+v", rec)
	}

	resultPath := filepath.Join(f.resultDir, contract.ResultFile)
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err = agent.ExtractResult(f.resultDir, schema)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != agent.StatusFailed || rec.Error.Reason != contract.ReasonBoxVanished {
		t.Fatalf("extract over truncated record = %+v", rec)
	}

	tampered := strings.Replace(string(raw), `"verdict": "ok"`, `"verdict": "forged"`, 1)
	if err := os.WriteFile(resultPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err = agent.ExtractResult(f.resultDir, schema)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != agent.StatusFailed || rec.Error.Reason != contract.ReasonOutputSchema || rec.Payload != nil {
		t.Fatalf("extract over tampered record = %+v", rec)
	}
}

// Edge case. Verifies ae434449cac9: unset FABER_EFFORT/FABER_MAX_BUDGET emit
// no flags; set, both appear with the exact values.
func TestLifecycle_EffortAndBudgetPassThrough(t *testing.T) {
	t.Run("unset omits the flags", func(t *testing.T) {
		f := newFixture(t)
		if code, log := f.run(); code != 0 {
			t.Fatalf("exit = %d\n%s", code, log)
		}
		joined := strings.Join(f.stubArgv(), "\x00")
		for _, flag := range []string{"--effort", "--max-budget-usd"} {
			if strings.Contains(joined, flag) {
				t.Errorf("argv carries %s though unset", flag)
			}
		}
	})
	t.Run("set passes exact values", func(t *testing.T) {
		f := newFixture(t)
		f.env[contract.EnvEffort] = "high"
		f.env[contract.EnvMaxBudget] = "2.50"
		if code, log := f.run(); code != 0 {
			t.Fatalf("exit = %d\n%s", code, log)
		}
		argv := f.stubArgv()
		joined := strings.Join(argv, "\x00")
		for _, want := range []string{"--effort\x00high", "--max-budget-usd\x002.50"} {
			if !strings.Contains(joined, want) {
				t.Errorf("argv = %q, want %q", argv, want)
			}
		}
	})
}
