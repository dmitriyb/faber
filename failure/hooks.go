package failure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"syscall"
	"time"
)

// HookInvocation is everything an on_failure cleanup can act on: the failed
// step's identity, the attempt it follows, the resolved input values, and the
// error record. It never carries engine internals.
type HookInvocation struct {
	Script  string
	StepID  string
	Attempt int
	Inputs  map[string]any
	Error   ErrorRecord
}

// HookRunner executes on_failure cleanup hooks. It is an interface so tests
// (and future execution surfaces) can substitute a fake; the production
// implementation is ExecHookRunner.
type HookRunner interface {
	// RunOnFailure runs the opaque user script host-side. A non-nil error
	// means the cleanup itself failed; the caller reports it but never lets
	// it mask the original failure.
	RunOnFailure(ctx context.Context, inv HookInvocation) error
}

// hookOutputCap bounds how much captured hook output travels in an error (and
// therefore into cleanup records).
const hookOutputCap = 4096

// ExecHookRunner runs hooks host-side via os/exec: the script is exec'd
// directly (no shell), receives the resolved inputs as FABER_INPUT_* env vars
// plus FABER_STEP_ID and FABER_ATTEMPT, and the error record as JSON on
// stdin. Its exit code is the only return.
type ExecHookRunner struct {
	Log *slog.Logger
}

// RunOnFailure implements HookRunner.
func (r *ExecHookRunner) RunOnFailure(ctx context.Context, inv HookInvocation) error {
	stdin, err := json.Marshal(inv.Error)
	if err != nil {
		return fmt.Errorf("failure: on_failure %s: encode error record: %w", inv.Script, err)
	}
	env := append(os.Environ(),
		"FABER_STEP_ID="+inv.StepID,
		"FABER_ATTEMPT="+strconv.Itoa(inv.Attempt),
	)
	slots := make([]string, 0, len(inv.Inputs))
	for name := range inv.Inputs {
		slots = append(slots, name)
	}
	sort.Strings(slots)
	mapped := make(map[string]string, len(slots)) // env name → slot it came from
	for _, name := range slots {
		v, err := envValue(inv.Inputs[name])
		if err != nil {
			return fmt.Errorf("failure: on_failure %s: input %s: %w", inv.Script, name, err)
		}
		key := envName(name)
		if prev, dup := mapped[key]; dup {
			// os/exec keeps the last duplicate, which would silently shadow a
			// resolved input from the cleanup — refuse instead.
			return fmt.Errorf("failure: on_failure %s: inputs %q and %q both map to env var FABER_INPUT_%s; rename one slot",
				inv.Script, prev, name, key)
		}
		mapped[key] = name
		env = append(env, "FABER_INPUT_"+key+"="+v)
	}

	cmd := exec.CommandContext(ctx, inv.Script)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(stdin)
	// The hook runs in its own process group so cancellation (first pass:
	// process-level abort) kills the whole script tree, and WaitDelay
	// backstops a child that keeps the output pipes open past the kill.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failure: on_failure %s (step %s, attempt %d): %w; output: %s",
			inv.Script, inv.StepID, inv.Attempt, err, tail(out, hookOutputCap))
	}
	if r.Log != nil {
		r.Log.Debug("on_failure hook completed", "script", inv.Script, "step", inv.StepID, "attempt", inv.Attempt)
	}
	return nil
}

// envName maps a slot name onto env-var alphabet: upper-cased, with every
// non-alphanumeric byte replaced by '_'.
func envName(slot string) string {
	b := []byte(slot)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 'a' + 'A'
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		default:
			b[i] = '_'
		}
	}
	return string(b)
}

// envValue renders one resolved input for the hook env: scalars verbatim,
// objects (and anything else) as JSON.
func envValue(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case int:
		return strconv.Itoa(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64), nil
	case json.Number:
		return t.String(), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// tail returns at most n trailing bytes of trimmed output.
func tail(out []byte, n int) []byte {
	out = bytes.TrimSpace(out)
	if len(out) <= n {
		return out
	}
	return out[len(out)-n:]
}
