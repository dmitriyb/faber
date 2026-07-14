package box

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dmitriyb/faber/agent/contract"
)

// runContextHook is phase 7: the first user-filled phase.
func (b *Box) runContextHook(ctx context.Context) error {
	return b.runHook(ctx, contract.HookContext)
}

// runPreludeHook is phase 8: the second user-filled phase, followed by the
// bundle postcondition — the agent never starts on a missing bundle. A
// hook-less template gets a synthesized minimal bundle so the agent phase
// sees one shape regardless of template.
func (b *Box) runPreludeHook(ctx context.Context) error {
	if err := b.runHook(ctx, contract.HookPrelude); err != nil {
		return err
	}
	if !b.hookDeclared {
		if err := synthesizeBundle(b.Env.BundleDir, b.Env.Inputs); err != nil {
			return &boxError{Reason: contract.ReasonBundleMissing, Detail: fmt.Sprintf("synthesize bundle: %v", err)}
		}
	}
	bundle, err := LoadBundle(b.Env.BundleDir)
	if err != nil {
		if errors.Is(err, ErrNoBundle) {
			return &boxError{
				Reason: contract.ReasonBundleMissing,
				Detail: fmt.Sprintf("hooks exited 0 but %s is missing or empty in the bundle directory", contract.ContextDoc),
			}
		}
		return &boxError{Reason: contract.ReasonBundleMalformed, Detail: err.Error()}
	}
	if _, ok := bundle.Env[contract.BranchKey]; ok && b.Env.RemoteURL == "" {
		return &boxError{
			Reason: contract.ReasonBundleMalformed,
			Detail: contract.BranchKey + " declared as a side-effect but the step binds no repo",
		}
	}
	// Sidecar values are merged into the agent's environment last, so a
	// reserved name would silently override the engine contract (result dir,
	// forwarded socket, ssh policy, PATH). Reject them here — the agent never
	// starts on a malformed bundle.
	for _, key := range slices.Sorted(maps.Keys(bundle.Env)) {
		if reservedSidecarKey(key) {
			return &boxError{
				Reason: contract.ReasonBundleMalformed,
				Detail: fmt.Sprintf("%s: key %q is engine- or runner-owned and may not be overridden", contract.BundleEnvFile, key),
			}
		}
	}
	b.Bundle = bundle
	return nil
}

// reservedSidecarKey reports whether a bundle.env key would override the
// engine/security env contract or the process runner environment.
func reservedSidecarKey(key string) bool {
	return contract.EngineOwnedEnv(key) || key == "PATH" || key == "GIT_SSH_COMMAND"
}

// runHook executes one opaque user hook. The environment is the whole
// interface: the hook inherits the box environment as assembled by the setup
// phases (typed inputs, engine variables, delegated handles) and runs in the
// workspace. A missing hook file means the template declared none — the
// phase is a no-op. Nonzero exit fail-stops the step: there is no partial
// credit and no in-box retry.
func (b *Box) runHook(ctx context.Context, name string) error {
	hookPath := filepath.Join(b.Env.HooksDir, name)
	if _, err := os.Stat(hookPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			b.Log.DebugContext(ctx, "hook not declared", "hook", name)
			return nil
		}
		return &boxError{Reason: contract.ReasonHookFailed, Detail: fmt.Sprintf("stat hook %s: %v", name, err)}
	}
	b.hookDeclared = true
	res, err := b.Runner.Stream(ctx, CmdSpec{
		Argv: []string{hookPath},
		Dir:  b.Workdir,
		Env:  b.Environ,
	})
	if err != nil {
		return &boxError{Reason: contract.ReasonHookFailed, Detail: fmt.Sprintf("run hook %s: %v", name, err)}
	}
	if res.ExitCode != 0 {
		return &boxError{
			Reason:     contract.ReasonHookFailed,
			Detail:     fmt.Sprintf("hook %s exited %d", name, res.ExitCode),
			ExitCode:   res.ExitCode,
			StderrTail: string(res.StderrTail),
		}
	}
	return nil
}

// ErrNoBundle marks a missing or empty context document after the hooks
// succeeded.
var ErrNoBundle = errors.New("box: no context bundle")

// Bundle is the loaded context bundle: the prompt-body document plus the
// optional machine-readable sidecar.
type Bundle struct {
	Dir string
	Doc string
	Env map[string]string
}

// LoadBundle enforces the bundle postcondition: CONTEXT.md must exist and be
// non-empty (a zero-byte document counts as missing); bundle.env, when
// present, must parse. Sidecar values are opaque bytes to the engine.
func LoadBundle(dir string) (*Bundle, error) {
	raw, err := os.ReadFile(filepath.Join(dir, contract.ContextDoc))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoBundle
		}
		return nil, fmt.Errorf("box: read %s: %w", contract.ContextDoc, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, ErrNoBundle
	}
	bundle := &Bundle{Dir: dir, Doc: string(raw), Env: map[string]string{}}
	sidecar, err := os.ReadFile(filepath.Join(dir, contract.BundleEnvFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return bundle, nil
		}
		return nil, fmt.Errorf("box: read %s: %w", contract.BundleEnvFile, err)
	}
	env, err := parseBundleEnv(string(sidecar))
	if err != nil {
		return nil, err
	}
	bundle.Env = env
	return bundle, nil
}

// parseBundleEnv is deliberately dumb: line-oriented KEY=VALUE, # comments
// and blank lines skipped, no quoting, no expansion. A malformed line is a
// prelude-phase contract error, not a warning.
func parseBundleEnv(content string) (map[string]string, error) {
	env := map[string]string{}
	for i, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, val, ok := strings.Cut(trimmed, "=")
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("box: %s line %d: not KEY=VALUE", contract.BundleEnvFile, i+1)
		}
		env[key] = val
	}
	return env, nil
}

// synthesizeBundle writes the minimal context document for a hook-less
// template: an enumeration of the step's typed inputs, sorted by name.
func synthesizeBundle(dir string, inputs map[string]string) error {
	var sb strings.Builder
	sb.WriteString("# Step inputs\n")
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		fmt.Fprintf(&sb, "\n- %s: %s\n", name, inputs[name])
	}
	if len(inputs) == 0 {
		sb.WriteString("\n(no bound inputs)\n")
	}
	return os.WriteFile(filepath.Join(dir, contract.ContextDoc), []byte(sb.String()), 0o644)
}
