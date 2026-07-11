package infra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/faber/config"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fullRunSpec is the golden-argv fixture: limits, both engine mounts, three
// env keys, and a representative binding fragment (--network, agent-socket
// -v, SSH_AUTH_SOCK, proxy env, :ro secret mount, --runtime).
func fullRunSpec() RunSpec {
	return RunSpec{
		Name:      "faber-r1-impl-a1",
		Image:     "faber/impl:0a1b2c3d4e5f",
		Resources: config.ResourceDef{Memory: "8g", CPUs: 4},
		Mounts: []Mount{
			{Host: "/runs/r1/impl", Container: "/result"},
			{Host: "/proj/hooks", Container: "/hooks", ReadOnly: true},
		},
		Env: map[string]string{
			"FABER_STEP":   "impl",
			"FABER_RESULT": "/result/result.json",
			"ANSWER":       "42",
		},
		Bindings: []string{
			"--network", "wf-net",
			"-v", "/run/faber/agent.sock:/agent.sock",
			"-e", "SSH_AUTH_SOCK=/agent.sock",
			"-e", "HTTPS_PROXY=http://egress:3128",
			"-v", "/dev/shm/h1:/creds/token:ro",
			"--runtime", "runsc",
		},
		Entry: []string{"/hooks/box-entry", "--phase", "all"},
	}
}

// Verifies 0c82c6478856: a fully populated RunSpec assembles to the golden
// argv byte-for-byte — fixed section order, sorted env, the binding fragment
// contiguous and verbatim at its slot, image then entry argv last.
func TestGoldenArgv(t *testing.T) {
	got := buildArgs(fullRunSpec())
	want := []string{
		"run", "--rm", "--name", "faber-r1-impl-a1",
		"--memory=8g", "--cpus=4",
		"-v", "/runs/r1/impl:/result",
		"-v", "/proj/hooks:/hooks:ro",
		"-e", "ANSWER=42",
		"-e", "FABER_RESULT=/result/result.json",
		"-e", "FABER_STEP=impl",
		"--network", "wf-net",
		"-v", "/run/faber/agent.sock:/agent.sock",
		"-e", "SSH_AUTH_SOCK=/agent.sock",
		"-e", "HTTPS_PROXY=http://egress:3128",
		"-v", "/dev/shm/h1:/creds/token:ro",
		"--runtime", "runsc",
		"faber/impl:0a1b2c3d4e5f",
		"/hooks/box-entry", "--phase", "all",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv mismatch:\n got %q\nwant %q", got, want)
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

		if args[0] != "run" || args[1] != "--rm" || args[2] != "--name" || args[3] != spec.Name {
			t.Fatalf("spec %d: argv head %q", i, args[:4])
		}
		joined := "\x00" + strings.Join(args, "\x00") + "\x00"
		for _, forbidden := range []string{"docker.sock", "--privileged", "--network=host", "--pid=host", "--ipc=host"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("spec %d: forbidden token %q in argv %q", i, forbidden, args)
			}
		}
		// Every -v traces to a declared mount or to the verbatim fragment.
		declared := map[string]bool{}
		for _, m := range spec.Mounts {
			v := m.Host + ":" + m.Container
			if m.ReadOnly {
				v += ":ro"
			}
			declared[v] = true
		}
		for j := 0; j < len(spec.Bindings)-1; j++ {
			if spec.Bindings[j] == "-v" {
				declared[spec.Bindings[j+1]] = true
			}
		}
		for j, a := range args {
			if a == "-v" {
				if j+1 >= len(args) || !declared[args[j+1]] {
					t.Fatalf("spec %d: undeclared -v %q", i, args[j+1])
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

func randomRunSpec(rng *rand.Rand, i int) RunSpec {
	spec := RunSpec{
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
	env := map[string]string{}
	for e := rng.Intn(4); e > 0; e-- {
		env[fmt.Sprintf("K%d", rng.Intn(100))] = fmt.Sprintf("v%d", rng.Intn(100))
	}
	spec.Env = env
	fragments := [][]string{
		{"--network", fmt.Sprintf("net-%d", rng.Intn(10))},
		{"-v", fmt.Sprintf("/run/faber/a%d.sock:/agent.sock", rng.Intn(10))},
		{"-e", "SSH_AUTH_SOCK=/agent.sock"},
		{"-v", fmt.Sprintf("/dev/shm/h%d:/creds/token:ro", rng.Intn(10))},
		{"--runtime", "runsc"},
	}
	for _, f := range fragments {
		if rng.Intn(2) == 0 {
			spec.Bindings = append(spec.Bindings, f...)
		}
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
	spec := RunSpec{Name: "faber-smoke", Image: "faber/t:abc"}
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
		runFn: func(ctx context.Context, args []string, output io.Writer) (int, error) {
			fmt.Fprintln(output, "box says goodbye")
			return 1, nil
		},
	}
	r := NewContainerRunner(docker, testLogger())
	res, err := r.Run(context.Background(), RunSpec{Name: "faber-x", Image: "faber/t:abc"})
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

// Verifies 0c82c6478856: on context cancellation Run kills the container by
// its deterministic name, returns the context error within the grace window,
// and the RunResult still carries partial output and timing.
func TestKillOnCancel(t *testing.T) {
	docker := &fakeDocker{
		runFn: func(ctx context.Context, args []string, output io.Writer) (int, error) {
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
	res, err := r.Run(ctx, RunSpec{Name: "faber-cancelme", Image: "faber/t:abc"})
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
	if _, err := r.Run(context.Background(), RunSpec{Image: "faber/t:abc"}); err == nil {
		t.Fatal("missing name accepted")
	}
	if _, err := r.Run(context.Background(), RunSpec{Name: "faber-x"}); err == nil {
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
