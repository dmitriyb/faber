package infra

// Recording fakes for the adapter interfaces: each appends the call's argv
// (or staged expression) to a slice and pops canned results. Components
// receive interfaces at construction, so no build tags or indirection are
// needed — the realinfra suite is the only place real CLIs run.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fakeDocker struct {
	mu         sync.Mutex
	calls      [][]string
	exists     map[string]bool  // ImageExists / NetworkExists answers
	existsHook func(tag string) // runs on ImageExists, before the answer (race windows)
	loadTag    string           // tag Load reports
	loadErr    error
	markLoaded bool // Load marks loadTag as existing (daemon-as-cache)
	runFn      func(ctx context.Context, args []string, stdin io.Reader, output io.Writer) (int, error)
	runStdins  [][]byte // full contents of each ContainerRun's stdin (nil when unattached)
	killErr    error
	killed     []string
}

func (f *fakeDocker) record(verb string, args ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{verb}, args...))
}

func (f *fakeDocker) callsFor(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeDocker) ImageExists(ctx context.Context, tag string) (bool, error) {
	f.record("image-exists", tag)
	if f.existsHook != nil {
		f.existsHook(tag)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exists[tag], nil
}

func (f *fakeDocker) Load(ctx context.Context, tarball string) (string, error) {
	f.record("load", tarball)
	if f.loadErr != nil {
		return "", f.loadErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markLoaded {
		if f.exists == nil {
			f.exists = map[string]bool{}
		}
		f.exists[f.loadTag] = true
	}
	return f.loadTag, nil
}

func (f *fakeDocker) NetworkExists(ctx context.Context, name string) (bool, error) {
	f.record("network-exists", name)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exists[name], nil
}

func (f *fakeDocker) ContainerRun(ctx context.Context, args []string, stdin io.Reader, output io.Writer) (int, error) {
	f.record("run", args...)
	// Drain stdin to EOF exactly as the real adapter does — the box reads the
	// clean EOF as end-of-payload. nil stdin records a nil entry.
	var drained []byte
	if stdin != nil {
		drained, _ = io.ReadAll(stdin)
	}
	f.mu.Lock()
	f.runStdins = append(f.runStdins, drained)
	f.mu.Unlock()
	if f.runFn != nil {
		return f.runFn(ctx, args, stdin, output)
	}
	return 0, nil
}

func (f *fakeDocker) Kill(ctx context.Context, name string) error {
	f.record("kill", name)
	f.mu.Lock()
	f.killed = append(f.killed, name)
	f.mu.Unlock()
	return f.killErr
}

func (f *fakeDocker) Remove(ctx context.Context, name string) error {
	f.record("remove", name)
	return nil
}

// fakeNix records the *contents* of each staged expression file (the file is
// deleted after the call) plus whether a staged overlay sat beside it.
type fakeNix struct {
	mu            sync.Mutex
	evalExprs     []string
	evalOverlays  []bool // overlay.nix staged beside the expr file?
	evalResults   []json.RawMessage
	evalErr       error
	buildExprs    []string
	buildOverlays [][]byte // staged overlay.nix bytes per Build (nil when absent)
	buildOut      []string
	buildErr      error
	buildDelay    time.Duration // widens the single-flight race window
}

func (f *fakeNix) Eval(ctx context.Context, exprFile string, args []string) (json.RawMessage, error) {
	expr, hasOverlay := readStaged(exprFile)
	f.mu.Lock()
	f.evalExprs = append(f.evalExprs, expr)
	f.evalOverlays = append(f.evalOverlays, hasOverlay)
	var res json.RawMessage
	if len(f.evalResults) > 0 {
		res = f.evalResults[0]
		if len(f.evalResults) > 1 {
			f.evalResults = f.evalResults[1:]
		}
	}
	err := f.evalErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, fmt.Errorf("fakeNix: no eval result staged")
	}
	return res, nil
}

func (f *fakeNix) Build(ctx context.Context, exprFile string) ([]string, error) {
	expr, _ := readStaged(exprFile)
	overlay, _ := os.ReadFile(filepath.Join(filepath.Dir(exprFile), stagedOverlayName))
	f.mu.Lock()
	f.buildExprs = append(f.buildExprs, expr)
	f.buildOverlays = append(f.buildOverlays, overlay)
	delay, out, err := f.buildDelay, f.buildOut, f.buildErr
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (f *fakeNix) evalCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.evalExprs)
}

func (f *fakeNix) buildCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.buildExprs)
}

func readStaged(exprFile string) (expr string, hasOverlay bool) {
	data, err := os.ReadFile(exprFile)
	if err != nil {
		return "", false
	}
	_, statErr := os.Stat(filepath.Join(filepath.Dir(exprFile), stagedOverlayName))
	return string(data), statErr == nil
}
