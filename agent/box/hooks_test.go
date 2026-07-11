package box

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmitriyb/faber/agent/contract"
)

// Verifies b880aa49b3b9: hooks receive the step's typed inputs as
// environment and run in the workspace cwd — the environment is the whole
// interface, there is no argv protocol.
func TestHookEnvironmentAndCwd(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	writeHook(t, d, contract.HookContext)
	b := newTestBox(t, d, map[string]string{"FABER_INPUT_ALPHA": "v1"}, fr)
	if err := b.checkEnv(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Workdir = d.workspace
	if err := b.runContextHook(context.Background()); err != nil {
		t.Fatal(err)
	}
	call := fr.calls[0]
	if len(call.Argv) != 1 {
		t.Fatalf("hook argv = %v, want the bare executable (env is the whole interface)", call.Argv)
	}
	if call.Dir != d.workspace {
		t.Fatalf("hook cwd = %q, want workspace", call.Dir)
	}
	if envLookup(call.Env, "FABER_INPUT_ALPHA") != "v1" {
		t.Fatal("hook env misses the typed input")
	}
	if !fr.streams[0] {
		t.Fatal("hook output must stream to the container log, not be parsed")
	}
}

// Verifies b880aa49b3b9: a nonzero hook exit aborts the step with a
// structured handoff record naming the phase, the exit code, and a stderr
// tail — the agent never starts.
func TestFailedPreludeAbortsBeforeAgent(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == filepath.Join(d.hooks, contract.HookPrelude) {
			return CmdResult{ExitCode: 3, StderrTail: []byte("prelude broke\n")}, nil
		}
		return CmdResult{}, nil
	}
	writeHook(t, d, contract.HookPrelude)
	b := newTestBox(t, d, nil, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	for _, call := range fr.argvs() {
		if strings.HasPrefix(call, "agent-cli") {
			t.Fatal("the agent started after a failed prelude")
		}
	}
	h := readHandoff(t, d)
	if h.Phase != "prelude" || h.Reason != contract.ReasonHookFailed || h.ExitCode != 3 {
		t.Fatalf("handoff = %+v", h)
	}
	if !strings.Contains(h.StderrTail, "prelude broke") {
		t.Fatalf("handoff stderr tail = %q", h.StderrTail)
	}
}

// Verifies b880aa49b3b9: hooks that exit 0 without producing the bundle
// abort with reason bundle-missing — the agent never starts on a missing
// bundle. An empty (zero-byte) CONTEXT.md counts as missing.
func TestBundleLessPreludeAborts(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, d testDirs)
	}{
		{"no context doc", func(t *testing.T, d testDirs) {}},
		{"zero-byte context doc", func(t *testing.T, d testDirs) {
			if err := os.WriteFile(filepath.Join(d.bundle, contract.ContextDoc), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newTestDirs(t)
			fr := &fakeRunner{}
			writeHook(t, d, contract.HookContext)
			writeHook(t, d, contract.HookPrelude)
			tt.setup(t, d)
			b := newTestBox(t, d, nil, fr)
			if code := Main(context.Background(), b); code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			h := readHandoff(t, d)
			if h.Phase != "prelude" || h.Reason != contract.ReasonBundleMissing {
				t.Fatalf("handoff = %+v, want prelude/bundle-missing", h)
			}
			for _, call := range fr.argvs() {
				if strings.HasPrefix(call, "agent-cli") {
					t.Fatal("the agent started without a bundle")
				}
			}
		})
	}
}

// Verifies b880aa49b3b9 and db9815889696 (first pass): a hook-less template
// still gets a uniform agent phase — the sequencer synthesizes a minimal
// CONTEXT.md enumerating the step's typed inputs; context gathering beyond
// that single opaque script seam is deferred.
func TestHookLessTemplateSynthesizesBundle(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == "agent-cli" {
			writeOutput(t, d, `{}`)
		}
		return CmdResult{}, nil
	}
	b := newTestBox(t, d, map[string]string{
		"FABER_INPUT_BETA":  "v2",
		"FABER_INPUT_ALPHA": "v1",
	}, fr)
	if code := Main(context.Background(), b); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	t.Cleanup(func() { os.RemoveAll(b.Workdir) })
	doc := b.Bundle.Doc
	alpha := strings.Index(doc, "ALPHA: v1")
	beta := strings.Index(doc, "BETA: v2")
	if alpha < 0 || beta < 0 || beta < alpha {
		t.Fatalf("synthesized doc must enumerate inputs sorted, got:\n%s", doc)
	}
	// The synthesized doc reached the agent as the prompt body.
	agentCall := fr.calls[len(fr.calls)-1]
	if !strings.Contains(agentCall.Argv[2], "ALPHA: v1") {
		t.Fatalf("agent prompt misses the synthesized bundle: %q", agentCall.Argv[2])
	}
}

// Verifies b880aa49b3b9: bundle.env parsing is line-oriented KEY=VALUE with
// comments and blanks skipped; a malformed line is a prelude-phase contract
// error, not an agent-phase one.
func TestBundleEnvParsing(t *testing.T) {
	t.Run("valid sidecar", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, contract.ContextDoc, "body\n")
		writeFile(t, dir, contract.BundleEnvFile, "# comment\n\nBRANCH=t-1\nREF=a=b\n")
		bundle, err := LoadBundle(dir)
		if err != nil {
			t.Fatal(err)
		}
		if bundle.Env["BRANCH"] != "t-1" || bundle.Env["REF"] != "a=b" {
			t.Fatalf("bundle env = %v", bundle.Env)
		}
	})
	t.Run("malformed line fails the prelude phase", func(t *testing.T) {
		d := newTestDirs(t)
		fr := &fakeRunner{}
		fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
			if spec.Argv[0] == filepath.Join(d.hooks, contract.HookPrelude) {
				writeFile(t, d.bundle, contract.ContextDoc, "body\n")
				writeFile(t, d.bundle, contract.BundleEnvFile, "not a pair\n")
			}
			return CmdResult{}, nil
		}
		writeHook(t, d, contract.HookPrelude)
		b := newTestBox(t, d, nil, fr)
		if code := Main(context.Background(), b); code != 1 {
			t.Fatal("malformed bundle.env must fail the step")
		}
		h := readHandoff(t, d)
		if h.Phase != "prelude" || h.Reason != contract.ReasonBundleMalformed {
			t.Fatalf("handoff = %+v, want prelude/bundle-malformed", h)
		}
	})
}

// Verifies b880aa49b3b9: a BRANCH declaration without a bound repo is a
// contract error caught at the prelude postcondition.
func TestBranchDeclarationWithoutRepo(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		if spec.Argv[0] == filepath.Join(d.hooks, contract.HookPrelude) {
			writeFile(t, d.bundle, contract.ContextDoc, "body\n")
			writeFile(t, d.bundle, contract.BundleEnvFile, "BRANCH=t-1\n")
		}
		return CmdResult{}, nil
	}
	writeHook(t, d, contract.HookPrelude)
	b := newTestBox(t, d, nil, fr) // no FABER_REMOTE_URL: gateless
	if code := Main(context.Background(), b); code != 1 {
		t.Fatal("BRANCH without a repo must fail the step")
	}
	h := readHandoff(t, d)
	if h.Phase != "prelude" || h.Reason != contract.ReasonBundleMalformed {
		t.Fatalf("handoff = %+v, want prelude/bundle-malformed", h)
	}
}

// Verifies b880aa49b3b9: the fail-stop path snapshots the bundle directory
// into the mounted result dir so container removal cannot lose it.
func TestFailStopSnapshotsBundle(t *testing.T) {
	d := newTestDirs(t)
	fr := &fakeRunner{}
	fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
		switch spec.Argv[0] {
		case filepath.Join(d.hooks, contract.HookPrelude):
			writeFile(t, d.bundle, contract.ContextDoc, "body\n")
			return CmdResult{}, nil
		case "agent-cli":
			return CmdResult{ExitCode: 17}, nil
		}
		return CmdResult{}, nil
	}
	writeHook(t, d, contract.HookPrelude)
	b := newTestBox(t, d, nil, fr)
	if code := Main(context.Background(), b); code != 1 {
		t.Fatal("want failure exit")
	}
	snap := filepath.Join(d.result, filepath.FromSlash(contract.HandoffBundleDir), contract.ContextDoc)
	raw, err := os.ReadFile(snap)
	if err != nil {
		t.Fatalf("bundle snapshot missing: %v", err)
	}
	if string(raw) != "body\n" {
		t.Fatalf("snapshot content = %q", raw)
	}
}

// Verifies b880aa49b3b9 and ae434449cac9: bundle sidecar values merge into
// the agent's environment last, so engine- and runner-owned names (the
// FABER_ contract, the forwarded socket, ssh policy, PATH) are rejected at
// the prelude postcondition — a prelude cannot redirect the result channel
// or the agent's toolchain.
func TestSidecarCannotOverrideEngineEnv(t *testing.T) {
	for _, key := range []string{"FABER_RESULT_DIR", "SSH_AUTH_SOCK", "PATH", "GIT_SSH_COMMAND", "FABER_INPUT_ALPHA"} {
		t.Run(key, func(t *testing.T) {
			d := newTestDirs(t)
			fr := &fakeRunner{}
			fr.handle = func(spec CmdSpec, stream bool) (CmdResult, error) {
				if spec.Argv[0] == filepath.Join(d.hooks, contract.HookPrelude) {
					writeFile(t, d.bundle, contract.ContextDoc, "body\n")
					writeFile(t, d.bundle, contract.BundleEnvFile, key+"=/elsewhere\n")
				}
				return CmdResult{}, nil
			}
			writeHook(t, d, contract.HookPrelude)
			b := newTestBox(t, d, nil, fr)
			if code := Main(context.Background(), b); code != 1 {
				t.Fatalf("a %s sidecar override must fail the step", key)
			}
			h := readHandoff(t, d)
			if h.Phase != "prelude" || h.Reason != contract.ReasonBundleMalformed {
				t.Fatalf("handoff = %+v, want prelude/bundle-malformed", h)
			}
			for _, call := range fr.argvs() {
				if strings.HasPrefix(call, "agent-cli") {
					t.Fatal("the agent started on a malformed bundle")
				}
			}
		})
	}
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
