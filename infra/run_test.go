package infra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// goldenStdinSecrets is the golden fixture's file-mode secrets payload; its
// bytes must never appear anywhere in the assembled argv.
var goldenStdinSecrets = []byte(`{"agent-api":"dG9rLXNlY3JldA=="}`)

// fullRunSpec is the golden-argv fixture: limits, every engine mount kind (a
// template bind rw, a :ro bind, a workspace volume, a /faber/result and a
// /tmp tmpfs), three env keys, a StdinSecrets payload, and a
// representative binding fragment (--network, the agent tmpfs, SSH_AUTH_SOCK,
// proxy env, --tmpfs /run/secrets, --runtime).
func fullRunSpec() ContainerSpec {
	return ContainerSpec{
		Name:      "faber-r1-impl-a1",
		Image:     "faber/impl:0a1b2c3d4e5f",
		Resources: config.ResourceDef{Memory: "8g", CPUs: 4},
		Mounts: []Mount{
			{Host: "/proj/cache", Container: "/cache"},
			{Host: "/proj/hooks", Container: "/hooks", ReadOnly: true},
			{Kind: KindVolume, Container: "/workspace"},
			{Kind: KindTmpfs, Container: "/faber/result"},
			{Kind: KindTmpfs, Container: "/tmp"},
		},
		Env: map[string]string{
			"FABER_STEP":   "impl",
			"FABER_RESULT": "/result/result.json",
			"ANSWER":       "42",
		},
		StdinSecrets: goldenStdinSecrets,
		Bindings: []string{
			"--network", "wf-net",
			"--tmpfs", "/faber/agent",
			"-e", "SSH_AUTH_SOCK=/faber/agent/agent.sock",
			"-e", "HTTPS_PROXY=http://egress:3128",
			"--tmpfs", "/run/secrets",
			"--runtime", "runsc",
		},
		Entry: []string{"/hooks/box-entry", "--phase", "all"},
	}
}

// Verifies 0c82c6478856: a fully populated ContainerSpec assembles to the golden
// argv byte-for-byte — fixed section order, -i immediately after --rm (because
// StdinSecrets is set), sorted env, the binding fragment contiguous and
// verbatim at its slot, image then entry argv last. The same spec with
// StdinSecrets empty assembles identically except no -i appears, and the token
// bytes never reach the argv.
func TestGoldenArgv(t *testing.T) {
	got := buildArgs(fullRunSpec())
	want := []string{
		"run", "--rm", "-i", "--name", "faber-r1-impl-a1",
		"--memory=8g", "--cpus=4",
		"-v", "/proj/cache:/cache",
		"-v", "/proj/hooks:/hooks:ro",
		"-v", "/workspace",
		"--tmpfs", "/faber/result",
		"--tmpfs", "/tmp",
		"-e", "ANSWER=42",
		"-e", "FABER_RESULT=/result/result.json",
		"-e", "FABER_STEP=impl",
		"--network", "wf-net",
		"--tmpfs", "/faber/agent",
		"-e", "SSH_AUTH_SOCK=/faber/agent/agent.sock",
		"-e", "HTTPS_PROXY=http://egress:3128",
		"--tmpfs", "/run/secrets",
		"--runtime", "runsc",
		"faber/impl:0a1b2c3d4e5f",
		"/hooks/box-entry", "--phase", "all",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch:\n got %q\nwant %q", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "dG9rLXNlY3JldA==") || strings.Contains(a, "tok-secret") {
			t.Fatalf("token bytes leaked into argv: %q", a)
		}
	}

	// Same spec with no StdinSecrets: identical except -i is gone.
	spec := fullRunSpec()
	spec.StdinSecrets = nil
	got = buildArgs(spec)
	if slices.Contains(got, "-i") {
		t.Fatalf("-i emitted with empty StdinSecrets: %q", got)
	}
	wantNoI := slices.DeleteFunc(slices.Clone(want), func(s string) bool { return s == "-i" })
	if !slices.Equal(got, wantNoI) {
		t.Fatalf("no-stdin argv mismatch:\n got %q\nwant %q", got, wantNoI)
	}
}

// Verifies 0c82c6478856: over randomized RunSpecs the assembled argv never
// contains a docker-socket mount, --privileged, host networking, or any -v
// not present in the spec's mounts or fragment; --rm and --name are always
// present; declared memory/cpus always surface, absent resources emit none;
// the binding fragment stays contiguous and verbatim.
func TestArgvDiscipline(t *testing.T) {
	rng := rand.New(rand.NewSource(20260710))
	for i := 0; i < 500; i++ {
		spec := randomRunSpec(rng, i)
		args := buildArgs(spec)

		// Head is run, --rm, then the lone -i exactly when StdinSecrets is set,
		// then --name <name>.
		wantI := len(spec.StdinSecrets) > 0
		rest := args[2:]
		if wantI {
			if args[2] != "-i" {
				t.Fatalf("spec %d: -i not right after --rm: %q", i, args[:5])
			}
			rest = args[3:]
		}
		if args[0] != "run" || args[1] != "--rm" || rest[0] != "--name" || rest[1] != spec.Name {
			t.Fatalf("spec %d: argv head %q", i, args[:min(6, len(args))])
		}
		if got := slices.Contains(args, "-i"); got != wantI {
			t.Fatalf("spec %d: -i presence %v, want %v (StdinSecrets len %d)", i, got, wantI, len(spec.StdinSecrets))
		}
		joined := "\x00" + strings.Join(args, "\x00") + "\x00"
		// The token bytes never surface in the argv.
		if len(spec.StdinSecrets) > 0 && strings.Contains(joined, string(spec.StdinSecrets)) {
			t.Fatalf("spec %d: StdinSecrets bytes leaked into argv", i)
		}
		for _, forbidden := range []string{"docker.sock", "--privileged", "--user", "--network=host", "--pid=host", "--ipc=host"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("spec %d: forbidden token %q in argv %q", i, forbidden, args)
			}
		}
		// Every -v / --tmpfs traces to a declared mount or the verbatim fragment.
		declaredV := map[string]bool{}
		declaredTmpfs := map[string]bool{}
		for _, m := range spec.Mounts {
			switch m.Kind {
			case KindTmpfs:
				declaredTmpfs[m.Container] = true
			case KindVolume:
				v := m.Container
				if m.ReadOnly {
					v += ":ro"
				}
				declaredV[v] = true
			default:
				v := m.Host + ":" + m.Container
				if m.ReadOnly {
					v += ":ro"
				}
				declaredV[v] = true
			}
		}
		for j := 0; j < len(spec.Bindings)-1; j++ {
			switch spec.Bindings[j] {
			case "-v":
				declaredV[spec.Bindings[j+1]] = true
			case "--tmpfs":
				declaredTmpfs[spec.Bindings[j+1]] = true
			}
		}
		for j, a := range args {
			switch a {
			case "-v":
				if j+1 >= len(args) || !declaredV[args[j+1]] {
					t.Fatalf("spec %d: undeclared -v %q", i, args[j+1])
				}
			case "--tmpfs":
				if j+1 >= len(args) || !declaredTmpfs[args[j+1]] {
					t.Fatalf("spec %d: undeclared --tmpfs %q", i, args[j+1])
				}
			}
		}
		// Resource flags mirror the spec exactly.
		hasMem := slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--memory=") })
		if hasMem != (spec.Resources.Memory != "") {
			t.Fatalf("spec %d: memory flag presence %v, declared %q", i, hasMem, spec.Resources.Memory)
		}
		hasCPU := slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--cpus=") })
		if hasCPU != (spec.Resources.CPUs != 0) {
			t.Fatalf("spec %d: cpus flag presence %v, declared %v", i, hasCPU, spec.Resources.CPUs)
		}
		// The fragment is contiguous and verbatim.
		if len(spec.Bindings) > 0 {
			frag := "\x00" + strings.Join(spec.Bindings, "\x00") + "\x00"
			if !strings.Contains(joined, frag) {
				t.Fatalf("spec %d: binding fragment not contiguous/verbatim in %q", i, args)
			}
		}
		// Image then entry close the argv.
		tail := append([]string{spec.Image}, spec.Entry...)
		if !slices.Equal(args[len(args)-len(tail):], tail) {
			t.Fatalf("spec %d: argv tail %q, want %q", i, args[len(args)-len(tail):], tail)
		}
	}
}

func randomRunSpec(rng *rand.Rand, i int) ContainerSpec {
	spec := ContainerSpec{
		Name:  fmt.Sprintf("faber-r%d-step-a%d", i, rng.Intn(3)+1),
		Image: fmt.Sprintf("faber/t%d:%012x", i, rng.Int63()),
	}
	if rng.Intn(2) == 0 {
		spec.Resources.Memory = fmt.Sprintf("%dg", rng.Intn(15)+1)
	}
	if rng.Intn(2) == 0 {
		spec.Resources.CPUs = float64(rng.Intn(8) + 1)
	}
	for m := rng.Intn(3); m > 0; m-- {
		spec.Mounts = append(spec.Mounts, Mount{
			Host:      fmt.Sprintf("/host/%d", rng.Intn(1000)),
			Container: fmt.Sprintf("/box/%d", rng.Intn(1000)),
			ReadOnly:  rng.Intn(2) == 0,
		})
	}
	if rng.Intn(2) == 0 {
		spec.Mounts = append(spec.Mounts, Mount{Kind: KindVolume, Container: fmt.Sprintf("/vol/%d", rng.Intn(1000)), ReadOnly: rng.Intn(2) == 0})
	}
	if rng.Intn(2) == 0 {
		spec.Mounts = append(spec.Mounts, Mount{Kind: KindTmpfs, Container: fmt.Sprintf("/tmpfs/%d", rng.Intn(1000))})
	}
	env := map[string]string{}
	for e := rng.Intn(4); e > 0; e-- {
		env[fmt.Sprintf("K%d", rng.Intn(100))] = fmt.Sprintf("v%d", rng.Intn(100))
	}
	spec.Env = env
	fragments := [][]string{
		{"--network", fmt.Sprintf("net-%d", rng.Intn(10))},
		{"-v", fmt.Sprintf("/run/faber/a%d.sock:/agent.sock", rng.Intn(10))},
		{"-e", "SSH_AUTH_SOCK=/agent.sock"},
		{"--tmpfs", "/run/secrets"},
		{"--runtime", "runsc"},
	}
	for _, f := range fragments {
		if rng.Intn(2) == 0 {
			spec.Bindings = append(spec.Bindings, f...)
		}
	}
	// A file-mode secrets payload rides stdin on roughly half the specs.
	if rng.Intn(2) == 0 {
		spec.StdinSecrets = []byte(fmt.Sprintf(`{"svc-%d":"dG9r"}`, rng.Intn(100)))
	}
	for e := rng.Intn(3); e > 0; e-- {
		spec.Entry = append(spec.Entry, fmt.Sprintf("arg%d", rng.Intn(100)))
	}
	spec.Entry = append([]string{"/hooks/entry"}, spec.Entry...)
	return spec
}

// Verifies 0c82c6478856 (edge): an empty binding fragment and empty entry
// argv still assemble a valid engine-only run — assembly appends nothing
// after the image tag.
func TestArgvEmptyBindingsAndEntry(t *testing.T) {
	spec := ContainerSpec{Name: "faber-smoke", Image: "faber/t:abc"}
	got := buildArgs(spec)
	want := []string{"run", "--rm", "--name", "faber-smoke", "faber/t:abc"}
	if !slices.Equal(got, want) {
		t.Fatalf("argv %q, want %q", got, want)
	}
}

// Verifies 0c82c6478856: a non-zero box exit is a result, not a Go error —
// Run returns err == nil, ExitCode == 1, output attached; classification
// belongs to the failure module.
func TestNonZeroExitIsData(t *testing.T) {
	docker := &fakeDocker{
		runFn: func(ctx context.Context, args []string, stdin io.Reader, stdout, output io.Writer) (int, error) {
			fmt.Fprintln(output, "box says goodbye")
			return 1, nil
		},
	}
	r := NewContainerRunner(docker, testLogger())
	res, err := r.Run(context.Background(), ContainerSpec{Name: "faber-x", Image: "faber/t:abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("exit code %d, want 1", res.ExitCode)
	}
	if !strings.Contains(string(res.Output), "box says goodbye") {
		t.Fatalf("output %q missing captured line", res.Output)
	}
	if res.Duration < 0 || res.Started.IsZero() {
		t.Fatalf("timing not captured: %+v", res)
	}
}

// Verifies 0c82c6478856 (scenario 11): with a StdinSecrets payload the fake
// DockerClient.ContainerRun receives a non-nil stdin reader whose full
// contents equal the payload bytes (and it observes EOF); -i is in the argv
// and the payload never appears in the recorded argv or the log. With empty
// StdinSecrets the recorded stdin is nil and no -i is emitted.
func TestStdinSecretsDelivery(t *testing.T) {
	payload := []byte(`{"agent-api":"dG9rLXNlY3JldA=="}`)
	docker := &fakeDocker{}
	r := NewContainerRunner(docker, testLogger())
	if _, err := r.Run(context.Background(), ContainerSpec{
		Name: "faber-s", Image: "faber/t:abc", StdinSecrets: payload,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(docker.runStdins) != 1 || docker.runStdins[0] == nil {
		t.Fatalf("stdin not delivered: %v", docker.runStdins)
	}
	if !slices.Equal(docker.runStdins[0], payload) {
		t.Fatalf("stdin bytes %q, want %q", docker.runStdins[0], payload)
	}
	run := docker.callsFor("run")[0]
	if !slices.Contains(run, "-i") {
		t.Fatalf("argv missing -i: %q", run)
	}
	if strings.Contains(strings.Join(run, "\x00"), string(payload)) {
		t.Fatalf("payload leaked into recorded argv: %q", run)
	}

	// Empty payload: stdin unattached, no -i.
	docker2 := &fakeDocker{}
	r2 := NewContainerRunner(docker2, testLogger())
	if _, err := r2.Run(context.Background(), ContainerSpec{Name: "faber-s2", Image: "faber/t:abc"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(docker2.runStdins) != 1 || docker2.runStdins[0] != nil {
		t.Fatalf("stdin should be nil with empty StdinSecrets: %v", docker2.runStdins)
	}
	if slices.Contains(docker2.callsFor("run")[0], "-i") {
		t.Fatalf("-i emitted with empty StdinSecrets")
	}
}

// Verifies 0c82c6478856: on context cancellation Run kills the container by
// its deterministic name, returns the context error within the grace window,
// and the ContainerResult still carries partial output and timing.
func TestKillOnCancel(t *testing.T) {
	docker := &fakeDocker{
		runFn: func(ctx context.Context, args []string, stdin io.Reader, stdout, output io.Writer) (int, error) {
			fmt.Fprintln(output, "partial output before cancel")
			<-ctx.Done()
			return -1, fmt.Errorf("infra: docker: %w", ctx.Err())
		},
	}
	r := NewContainerRunner(docker, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	res, err := r.Run(ctx, ContainerSpec{Name: "faber-cancelme", Image: "faber/t:abc"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	if !slices.Contains(docker.killed, "faber-cancelme") {
		t.Fatalf("container not killed by name; kills: %v", docker.killed)
	}
	if !strings.Contains(string(res.Output), "partial output") {
		t.Fatalf("partial output lost: %q", res.Output)
	}
	if res.Duration <= 0 {
		t.Fatalf("duration not captured: %v", res.Duration)
	}
}

// Verifies 0c82c6478856: Run rejects a spec without the deterministic name or
// image tag instead of emitting a malformed argv.
func TestRunSpecRequiresNameAndImage(t *testing.T) {
	docker := &fakeDocker{}
	r := NewContainerRunner(docker, testLogger())
	if _, err := r.Run(context.Background(), ContainerSpec{Image: "faber/t:abc"}); err == nil {
		t.Fatal("missing name accepted")
	}
	if _, err := r.Run(context.Background(), ContainerSpec{Name: "faber-x"}); err == nil {
		t.Fatal("missing image accepted")
	}
	if got := len(docker.calls); got != 0 {
		t.Fatalf("docker touched %d times on invalid specs", got)
	}
}

// Verifies 0c82c6478856 (edge): output beyond the cap keeps the tail, head
// discarded, with no allocation blow-up on a log-spamming box.
func TestTailBufferKeepsTail(t *testing.T) {
	tb := newTailBuffer(16)
	for i := 0; i < 100; i++ {
		fmt.Fprintf(tb, "chunk-%02d;", i)
	}
	got := string(tb.Bytes())
	if len(got) != 16 {
		t.Fatalf("retained %d bytes, want 16 (%q)", len(got), got)
	}
	if !strings.HasSuffix(got, "chunk-99;") {
		t.Fatalf("tail %q does not end with the last write", got)
	}
	// A single write larger than the cap keeps only its tail.
	tb2 := newTailBuffer(8)
	tb2.Write([]byte("0123456789abcdef"))
	if got := string(tb2.Bytes()); got != "89abcdef" {
		t.Fatalf("oversized write retained %q", got)
	}
}

// Verifies 0c82c6478856: before launch, Run evicts any stale holder of the
// deterministic container name (docker rm --force, idempotent) — a crashed
// prior process leaves a container that both blocks the name and keeps
// writing into the old attempt dir. The eviction precedes the run call.
func TestRunEvictsStaleNameHolder(t *testing.T) {
	docker := &fakeDocker{}
	r := NewContainerRunner(docker, testLogger())
	if _, err := r.Run(context.Background(), ContainerSpec{Name: "faber-e", Image: "faber/t:abc"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	removes := docker.callsFor("remove")
	if len(removes) != 1 || removes[0][1] != "faber-e" {
		t.Fatalf("want one pre-launch eviction of faber-e, got %v", removes)
	}
	// Ordering: the eviction is recorded before the run.
	var order []string
	for _, c := range docker.calls {
		if c[0] == "remove" || c[0] == "run" {
			order = append(order, c[0])
		}
	}
	if len(order) != 2 || order[0] != "remove" || order[1] != "run" {
		t.Fatalf("eviction must precede launch, got %v", order)
	}
}

// Verifies 0c82c6478856 (L-P3j): a relative bind host is refused before
// launch — docker would silently read it as a named volume and detach the
// result channel.
func TestRunRejectsRelativeBindHost(t *testing.T) {
	r := NewContainerRunner(&fakeDocker{}, testLogger())
	_, err := r.Run(context.Background(), ContainerSpec{
		Name: "faber-rel", Image: "faber/t:abc",
		Mounts: []Mount{{Host: ".faber/runs/r1/result", Container: "/faber/result"}},
	})
	if err == nil || !strings.Contains(err.Error(), "named volume") {
		t.Fatalf("want relative-host refusal, got %v", err)
	}
}

// Verifies 0c82c6478856 (scenario 12): the container's two streams reach
// two writers — stderr in full into Output and byte-identical to the bounded
// tail, stdout in full into Stdout — and neither sees the other's bytes; a
// nil Stdout discards the stream and leaves Output unchanged.
func TestRunTwoWriters(t *testing.T) {
	docker := &fakeDocker{
		runFn: func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			fmt.Fprintln(stderr, "line one")
			fmt.Fprint(stdout, "\x00faber\x00end\x00\x000\x000\n")
			fmt.Fprintln(stderr, "line two")
			return 0, nil
		},
	}
	var durable, frames bytes.Buffer
	r := NewContainerRunner(docker, testLogger())
	res, err := r.Run(context.Background(), ContainerSpec{Name: "faber-x", Image: "faber/t:abc", Output: &durable, Stdout: &frames})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if durable.String() != "line one\nline two\n" {
		t.Fatalf("durable writer got %q", durable.String())
	}
	if string(res.Output) != durable.String() {
		t.Fatalf("tail %q diverged from the durable copy %q", res.Output, durable.String())
	}
	if frames.String() != "\x00faber\x00end\x00\x000\x000\n" {
		t.Fatalf("stdout writer got %q", frames.String())
	}
	durable.Reset()
	if _, err := r.Run(context.Background(), ContainerSpec{Name: "faber-y", Image: "faber/t:abc", Output: &durable}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if durable.String() != "line one\nline two\n" {
		t.Fatalf("with Stdout nil, Output got %q", durable.String())
	}
}

// Verifies b8db21752444 (scenario 13): the exec argv shapes — -i exactly
// when a stdin is attached; the terminal variant with -it, user, workdir and
// the env sorted.
func TestExecArgs(t *testing.T) {
	if got := execArgs("faber-x", []string{"/faber/bin/faber-box", "receive"}, true); !slices.Equal(got, []string{"exec", "-i", "faber-x", "/faber/bin/faber-box", "receive"}) {
		t.Fatalf("exec with stdin = %q", got)
	}
	if got := execArgs("faber-x", []string{"stat", "/x"}, false); !slices.Equal(got, []string{"exec", "faber-x", "stat", "/x"}) {
		t.Fatalf("exec without stdin = %q", got)
	}
	got := execTerminalArgs("faber-x", []string{"/bin/sh"}, map[string]string{"HOME": "/home/box", "A": "1"}, "1000:1000", "/workspace")
	want := []string{"exec", "-it", "-u", "1000:1000", "-w", "/workspace", "-e", "A=1", "-e", "HOME=/home/box", "faber-x", "/bin/sh"}
	if !slices.Equal(got, want) {
		t.Fatalf("terminal exec = %q, want %q", got, want)
	}
	if got := execTerminalArgs("faber-x", []string{"/bin/sh"}, nil, "", ""); !slices.Equal(got, []string{"exec", "-it", "faber-x", "/bin/sh"}) {
		t.Fatalf("bare terminal exec = %q", got)
	}
}

// Verifies 0c82c6478856 (scenario 14): Enter issues remove, a detached run
// of the held box (no -i), the receive exec with the input drained, the
// terminal exec as the session says, and the kill — and the kill still runs
// when the receive fails (no attach) or the attach fails.
func TestEnter(t *testing.T) {
	spec := ContainerSpec{Name: "faber-i", Image: "faber/t:abc", StdinSecrets: []byte("never"),
		Mounts: []Mount{{Kind: KindTmpfs, Container: "/faber/handoff"}}, Entry: []string{"ignored"}}
	session := Session{Entry: []string{"harness", "resume"}, Env: map[string]string{"HOME": "/home/box"}, User: "1000:1000", Workdir: "/workspace"}
	verbs := func(d *fakeDocker) []string {
		var out []string
		for _, c := range d.calls {
			out = append(out, c[0])
		}
		return out
	}

	docker := &fakeDocker{}
	r := NewContainerRunner(docker, testLogger())
	if err := r.Enter(context.Background(), spec, strings.NewReader("the stream"), session); err != nil {
		t.Fatalf("enter: %v", err)
	}
	if got := verbs(docker); !slices.Equal(got, []string{"remove", "run", "exec", "exec-terminal", "kill"}) {
		t.Fatalf("verbs = %q", got)
	}
	run := docker.callsFor("run")[0]
	if !slices.Equal(run[:4], []string{"run", "run", "--rm", "-d"}) || slices.Contains(run, "-i") {
		t.Fatalf("held run argv = %q", run)
	}
	if !slices.Equal(run[len(run)-2:], []string{"/faber/bin/faber-box", "hold"}) || !slices.Contains(run, "--tmpfs") {
		t.Fatalf("held run argv = %q, want the hold entry and the tmpfs", run)
	}
	if ex := docker.callsFor("exec")[0]; !slices.Equal(ex, []string{"exec", "faber-i", "/faber/bin/faber-box", "receive"}) || string(docker.execStdins[0]) != "the stream" {
		t.Fatalf("receive exec = %q stdin %q", ex, docker.execStdins)
	}
	if term := docker.callsFor("exec-terminal")[0]; !slices.Equal(term, []string{"exec-terminal", "faber-i", "1000:1000", "/workspace", "harness", "resume"}) {
		t.Fatalf("terminal exec = %q", term)
	}

	docker = &fakeDocker{execFn: func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) (int, error) { return 3, nil }}
	r = NewContainerRunner(docker, testLogger())
	if err := r.Enter(context.Background(), spec, strings.NewReader(""), session); err == nil || !strings.Contains(err.Error(), "receive exited 3") {
		t.Fatalf("failed receive: %v", err)
	}
	if got := verbs(docker); !slices.Equal(got, []string{"remove", "run", "exec", "kill"}) {
		t.Fatalf("verbs after a failed receive = %q", got)
	}

	docker = &fakeDocker{termErr: errors.New("exit status 130")}
	r = NewContainerRunner(docker, testLogger())
	if err := r.Enter(context.Background(), spec, strings.NewReader(""), session); err == nil {
		t.Fatal("attach error swallowed")
	}
	if got := verbs(docker); got[len(got)-1] != "kill" {
		t.Fatalf("no kill after a failed attach: %q", got)
	}
}

// Verifies 0c82c6478856 (test scenario 15): Run starts the spec's attach
// with the container's name while the container runs and cancels it once
// the run returns; the attach's error never becomes the run's; a spec
// without one starts nothing.
func TestRunAttachLifecycle(t *testing.T) {
	attached := make(chan string, 1)
	docker := &fakeDocker{runFn: func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		select {
		case name := <-attached:
			attached <- name // leave it for the assertion
		case <-time.After(5 * time.Second):
			return 1, errors.New("the attach never ran while the container did")
		}
		return 0, nil
	}}
	var stopped atomic.Bool
	spec := ContainerSpec{Name: "faber-a", Image: "faber/t:abc", Attach: func(ctx context.Context, container string) error {
		attached <- container
		<-ctx.Done()
		stopped.Store(true)
		return ctx.Err()
	}}
	var logBuf bytes.Buffer
	r := NewContainerRunner(docker, slog.New(slog.NewTextHandler(&logBuf, nil)))
	res, err := r.Run(context.Background(), spec)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("run: %v exit %d", err, res.ExitCode)
	}
	if name := <-attached; name != "faber-a" {
		t.Fatalf("attach got container %q", name)
	}
	if !stopped.Load() {
		t.Fatal("the attach was not cancelled when the run returned")
	}
	if strings.Contains(logBuf.String(), "attach ended") {
		t.Fatalf("the cancellation was logged as a failure: %s", logBuf.String())
	}

	// A companion that fails on its own is a warning, not the run's error.
	docker = &fakeDocker{}
	logBuf.Reset()
	spec.Attach = func(context.Context, string) error { return errors.New("relay refused") }
	r = NewContainerRunner(docker, slog.New(slog.NewTextHandler(&logBuf, nil)))
	if _, err := r.Run(context.Background(), spec); err != nil {
		t.Fatalf("attach failure leaked into the run: %v", err)
	}
	if !strings.Contains(logBuf.String(), "relay refused") {
		t.Fatalf("attach failure not logged: %s", logBuf.String())
	}
}

// Verifies 0c82c6478856 (test scenario 15): Enter starts the attach after
// the held box is up and before the receive, and ends it before the kill.
func TestEnterStartsAttach(t *testing.T) {
	docker := &fakeDocker{}
	spec := ContainerSpec{Name: "faber-i", Image: "faber/t:abc", Attach: func(ctx context.Context, container string) error {
		docker.record("attach", container)
		<-ctx.Done()
		docker.record("attach-stopped")
		return ctx.Err()
	}}
	r := NewContainerRunner(docker, testLogger())
	if err := r.Enter(context.Background(), spec, strings.NewReader("s"), Session{Entry: []string{"sh"}}); err != nil {
		t.Fatalf("enter: %v", err)
	}
	var verbs []string
	for _, c := range docker.calls {
		verbs = append(verbs, c[0])
	}
	// The attach runs concurrently with the receive and the terminal exec
	// (instant in the fake), so its first record may land anywhere after
	// the run; what is fixed is: after the run, stopped before the kill.
	idx := func(v string) int { return slices.Index(verbs, v) }
	if !(idx("run") < idx("attach") && idx("attach") < idx("attach-stopped") && idx("attach-stopped") < idx("kill")) {
		t.Fatalf("verbs = %q", verbs)
	}
	if got := verbs[len(verbs)-1]; got != "kill" {
		t.Fatalf("last verb = %q", got)
	}
}

// Verifies b8db21752444 (test scenario 15): ExecWhenRunning polls the
// running query until true and execs exactly once; a context that ends
// while polling returns its error with no exec.
func TestExecWhenRunning(t *testing.T) {
	polls := 0
	docker := &fakeDocker{runningFn: func(string) (bool, error) {
		polls++
		return polls >= 3, nil
	}}
	code, err := ExecWhenRunning(context.Background(), docker, "faber-a", []string{"/faber/bin/faber-box", "relay"}, strings.NewReader(""), io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("exec: code %d err %v", code, err)
	}
	if polls != 3 || len(docker.callsFor("exec")) != 1 {
		t.Fatalf("polls %d, execs %d", polls, len(docker.callsFor("exec")))
	}
	if ex := docker.callsFor("exec")[0]; !slices.Equal(ex, []string{"exec", "faber-a", "/faber/bin/faber-box", "relay"}) {
		t.Fatalf("exec argv = %q", ex)
	}

	docker = &fakeDocker{runningFn: func(string) (bool, error) { return false, nil }}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if _, err := ExecWhenRunning(ctx, docker, "faber-a", nil, nil, io.Discard, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
	if len(docker.callsFor("exec")) != 0 {
		t.Fatal("exec ran against a container that never came up")
	}
}

// Verifies b8db21752444: the running query decodes the one JSON boolean.
func TestParseJSONBool(t *testing.T) {
	for raw, want := range map[string]bool{"true\n": true, "false": false} {
		if got, err := parseJSONBool([]byte(raw)); err != nil || got != want {
			t.Fatalf("parseJSONBool(%q) = %v, %v", raw, got, err)
		}
	}
	if _, err := parseJSONBool([]byte(`"running"`)); err == nil {
		t.Fatal("a non-boolean must not parse")
	}
}
