package metering

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// probeKillGrace is how long a cancelled probe gets between SIGTERM to its
// process group and SIGKILL escalation — a hung tokenizer or probe must not
// block shutdown.
const probeKillGrace = 10 * time.Second

// ExecRunner is the production ProbeRunner: it invokes an opaque user command
// via os/exec with an explicit argument list (never a shell), feeds it stdin,
// and returns captured stdout for structured decoding by the caller. Stderr
// is folded into the error for diagnosability — argv values are user config
// and may be sensitive, so errors carry the command name only.
type ExecRunner struct{}

// Run implements ProbeRunner.
func (ExecRunner) Run(ctx context.Context, argv []string, stdin io.Reader) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("metering: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	armProbe(cmd)
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("metering: run %s: %w: %s", argv[0], err, msg)
		}
		return nil, fmt.Errorf("metering: run %s: %w", argv[0], err)
	}
	return out, nil
}

// armProbe gives the probe its own process group, SIGTERMs the whole group on
// context cancellation, and escalates to SIGKILL after the grace period —
// the same treatment infra.setupProcess gives every other opaque subprocess
// (duplicated here because metering depends only on config; the two must
// stay behaviorally identical).
func armProbe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return os.ErrProcessDone
			}
			return err
		}
		return nil
	}
	// WaitDelay's SIGKILL reaches only the direct child; a SIGTERM-ignoring
	// grandchild survives it, but the force-closed pipes still unblock Wait,
	// so shutdown completes regardless.
	cmd.WaitDelay = probeKillGrace
}
