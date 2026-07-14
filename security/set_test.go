package security

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Verifies 41bb811a34e8 (and the composition of 6e6d0bb46819, 14a0498eb362,
// e47a00273f03, 0c5bc0f678b7): assembling the reference implement step yields
// the exact ordered fragment — network flags, proxy env, NO_PROXY list,
// remote URL env with the repo param spliced, pinned host-key env, socket
// mount + SSH_AUTH_SOCK, service handle mount, no runtime flag — byte
// identical across repeated assembly; adding runtime: runsc appends exactly
// --runtime=runsc and changes nothing else — test scenario 1.
func TestGoldenFragment(t *testing.T) {
	h := newHarness(t)
	scratch := t.TempDir()
	step := implementStep(scratch)
	want := implementFragment(scratch)

	first, err := h.set.Prepare(context.Background(), step)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !slices.Equal(first.Args, want) {
		t.Fatalf("fragment:\nwant %q\ngot  %q", want, first.Args)
	}
	// The file-mode token rides SecretsStdin as one JSON object, not the argv.
	wantPayload := `{"agent-api":"` + base64.StdEncoding.EncodeToString([]byte(testToken)) + `"}`
	if string(first.SecretsStdin) != wantPayload {
		t.Fatalf("SecretsStdin: want %q, got %q", wantPayload, first.SecretsStdin)
	}
	if err := first.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	second, err := h.set.Prepare(context.Background(), step)
	if err != nil {
		t.Fatalf("re-Prepare: %v", err)
	}
	if strings.Join(second.Args, "\x00") != strings.Join(first.Args, "\x00") {
		t.Fatalf("fragment not byte-identical across assemblies:\n%q\n%q", first.Args, second.Args)
	}
	if err := second.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	step.Runtime = "runsc"
	hardened, err := h.set.Prepare(context.Background(), step)
	if err != nil {
		t.Fatalf("Prepare with runtime: %v", err)
	}
	if !slices.Equal(hardened.Args, append(slices.Clone(want), "--runtime=runsc")) {
		t.Fatalf("runtime knob must append exactly --runtime=runsc:\ngot %q", hardened.Args)
	}
	if err := hardened.Teardown(context.Background()); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
}

// recBinding is an instrumented Binding recording setup/teardown call order.
type recBinding struct {
	name    string
	rec     *[]string
	prepErr error
	tdErr   error
}

func (r recBinding) Name() string { return r.name }

func (r recBinding) Prepare(context.Context, StepSpec) (Contribution, error) {
	*r.rec = append(*r.rec, "setup:"+r.name)
	if r.prepErr != nil {
		return Contribution{}, r.prepErr
	}
	return Contribution{
		Args: []string{"--" + r.name},
		Teardown: func(context.Context) error {
			*r.rec = append(*r.rec, "teardown:"+r.name)
			return r.tdErr
		},
	}, nil
}

func recSet(rec *[]string, override map[string]recBinding) *BindingSet {
	names := []string{"network", "remote", "identity", "credentials", "runtime"}
	bindings := make([]Binding, 0, len(names))
	for _, n := range names {
		b, ok := override[n]
		if !ok {
			b = recBinding{name: n, rec: rec}
		}
		bindings = append(bindings, b)
	}
	return &BindingSet{bindings: bindings, logger: slog.New(slog.DiscardHandler)}
}

// Verifies 41bb811a34e8: setup runs in the fixed order network, remote,
// identity, credentials, runtime; teardown runs strictly reversed; a teardown
// error in the credentials hook does not prevent the identity teardown and
// both errors surface joined — test scenario 7.
func TestUnwindOrderAndJoinedErrors(t *testing.T) {
	var rec []string
	set := recSet(&rec, map[string]recBinding{
		"identity":    {name: "identity", rec: &rec, tdErr: errors.New("identity teardown failed")},
		"credentials": {name: "credentials", rec: &rec, tdErr: errors.New("credentials teardown failed")},
	})
	a, err := set.Prepare(context.Background(), StepSpec{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wantSetup := []string{"setup:network", "setup:remote", "setup:identity", "setup:credentials", "setup:runtime"}
	if !slices.Equal(rec, wantSetup) {
		t.Fatalf("setup order: want %q, got %q", wantSetup, rec)
	}
	terr := a.Teardown(context.Background())
	wantAll := append(slices.Clone(wantSetup),
		"teardown:runtime", "teardown:credentials", "teardown:identity", "teardown:remote", "teardown:network")
	if !slices.Equal(rec, wantAll) {
		t.Fatalf("teardown order: want %q, got %q", wantAll, rec)
	}
	errContains(t, terr, "credentials teardown failed")
	errContains(t, terr, "identity teardown failed")
	errContains(t, terr, "security: teardown identity")
}

// Verifies 41bb811a34e8: a binding's Prepare failure stops assembly, unwinds
// only the already-prepared bindings in reverse, and surfaces an error naming
// the failing binding.
func TestPrepareFailureUnwindsPriorBindings(t *testing.T) {
	var rec []string
	set := recSet(&rec, map[string]recBinding{
		"credentials": {name: "credentials", rec: &rec, prepErr: errBoom},
	})
	_, err := set.Prepare(context.Background(), StepSpec{})
	errContains(t, err, "security: binding credentials")
	want := []string{
		"setup:network", "setup:remote", "setup:identity", "setup:credentials",
		"teardown:identity", "teardown:remote", "teardown:network",
	}
	if !slices.Equal(rec, want) {
		t.Fatalf("unwind: want %q, got %q", want, rec)
	}
}

// Verifies e47a00273f03 and 0c5bc0f678b7: teardown always runs — after a
// clean run, after a failed run, and when a later binding's Prepare fails
// after the agent was spawned (a failing resolver) — leaving the agent dead
// and the socket directory removed. File mode contributes no teardown and
// writes no host file, so there is nothing to shred; the (Prepare-failure)
// case additionally asserts no container ever started — test scenario 3.
func TestTeardownAlwaysRuns(t *testing.T) {
	assertClean := func(t *testing.T, h *harness, scratch string) {
		t.Helper()
		if live := h.agent.liveAgents(); len(live) != 0 {
			t.Fatalf("agent leaked: %q", live)
		}
		if _, err := os.Stat(filepath.Join(scratch, "ssh-agent")); !os.IsNotExist(err) {
			t.Fatal("socket directory must be removed")
		}
		// File mode writes no host file at all: the scratch area never held the
		// token, so there is nothing to shred.
		if _, err := os.Stat(filepath.Join(scratch, "agent-api")); !os.IsNotExist(err) {
			t.Fatal("file mode must not write a host token file")
		}
	}

	// Container exit 0 and non-zero look identical to the BindingSet: the
	// run happened, teardown follows.
	for _, exit := range []string{"container exit 0", "container exit non-zero"} {
		t.Run(exit, func(t *testing.T) {
			h := newHarness(t)
			scratch := t.TempDir()
			a, err := h.set.Prepare(context.Background(), implementStep(scratch))
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if err := a.Teardown(context.Background()); err != nil {
				t.Fatalf("Teardown: %v", err)
			}
			assertClean(t, h, scratch)
		})
	}

	t.Run("context cancelled mid-run", func(t *testing.T) {
		h := newHarness(t)
		scratch := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		a, err := h.set.Prepare(ctx, implementStep(scratch))
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		cancel() // the user abort arrives while the container runs
		if err := a.Teardown(ctx); err != nil {
			t.Fatalf("Teardown on cancelled ctx: %v", err)
		}
		assertClean(t, h, scratch)
		// The detached teardown context means the agent Stop saw a live
		// context despite the cancelled step — test edge "cancellation
		// during teardown".
		for _, seen := range h.agent.stopCtx {
			if seen != nil {
				t.Fatalf("teardown ran under a cancelled context: %v", seen)
			}
		}
	})

	t.Run("later binding Prepare fails", func(t *testing.T) {
		h := newHarness(t)
		scratch := t.TempDir()
		h.resolver.globalErr = errBoom // credentials fail after identity spawned the agent
		_, err := h.set.Prepare(context.Background(), implementStep(scratch))
		errContains(t, err, "security: binding credentials")
		assertClean(t, h, scratch)
		if h.docker.runCalls != 0 {
			t.Fatalf("no container may start after a failed assembly, got %d runs", h.docker.runCalls)
		}
	})
}

// Verifies 41bb811a34e8: an empty BindingSet (no network, remote, services,
// identity, or runtime) yields an empty fragment and a nil-safe teardown —
// the step is still runnable — test edge case.
func TestEmptyBindingSetIsLegal(t *testing.T) {
	h := newHarness(t)
	a, err := h.set.Prepare(context.Background(), StepSpec{NodeID: "task/bare", ScratchDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(a.Args) != 0 {
		t.Fatalf("want empty fragment, got %q", a.Args)
	}
	if a.Teardown == nil {
		t.Fatal("Teardown must never be nil")
	}
	if err := a.Teardown(context.Background()); err != nil {
		t.Fatalf("empty teardown: %v", err)
	}
}

// Verifies e47a00273f03 and 0157d30de15f (first pass only): retry gets a
// completely fresh assembly — a second attempt against its own scratch dir
// uses a different socket path and a fresh resolver invocation. That fresh
// invocation is the entire first-pass refresh story for expired credentials;
// no mid-run detection or refresh machinery exists — test edge case.
func TestRetryReassemblesFresh(t *testing.T) {
	h := newHarness(t)
	scratchA, scratchB := t.TempDir(), t.TempDir()

	first, err := h.set.Prepare(context.Background(), implementStep(scratchA))
	if err != nil {
		t.Fatalf("attempt 1 Prepare: %v", err)
	}
	if err := first.Teardown(context.Background()); err != nil { // attempt 1 failed; run its teardown
		t.Fatalf("attempt 1 Teardown: %v", err)
	}
	second, err := h.set.Prepare(context.Background(), implementStep(scratchB))
	if err != nil {
		t.Fatalf("attempt 2 Prepare: %v", err)
	}
	defer func() {
		if err := second.Teardown(context.Background()); err != nil {
			t.Fatalf("attempt 2 Teardown: %v", err)
		}
	}()

	if len(h.agent.starts) != 2 || h.agent.starts[0] == h.agent.starts[1] {
		t.Fatalf("attempts must use distinct sockets, got %q", h.agent.starts)
	}
	if h.resolver.callCount() != 2 {
		t.Fatalf("each attempt must invoke the resolver afresh, got %d calls", h.resolver.callCount())
	}
}

// Verifies 0c5bc0f678b7: a resolver still running when the step is cancelled
// aborts assembly and unwinds the already-spawned agent — the slow-resolver
// cancellation case.
func TestCancelledResolverUnwindsAgent(t *testing.T) {
	h := newHarness(t)
	h.resolver.blockCtx = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := h.set.Prepare(ctx, implementStep(t.TempDir()))
		if err == nil {
			t.Error("Prepare must fail when the resolver is cancelled")
		}
	}()
	cancel()
	<-done
	if live := h.agent.liveAgents(); len(live) != 0 {
		t.Fatalf("cancelled assembly leaked an agent: %q", live)
	}
}
