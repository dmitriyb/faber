package infra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// killGrace is how long a cancelled subprocess gets between SIGTERM to its
// process group and SIGKILL escalation, so a hung user command or wedged CLI
// cannot outlive its step.
const killGrace = 10 * time.Second

// setupProcess arms a command for group cancellation: the child gets its own
// process group, context cancellation SIGTERMs the whole group, and WaitDelay
// escalates to SIGKILL after the grace period.
func setupProcess(cmd *exec.Cmd) {
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
	cmd.WaitDelay = killGrace
}

// cliRunner is the shared exec core of the docker/git/nix adapters: one
// explicit argv (never sh -c), captured stdout, bounded stderr tail, context
// cancellation via process-group SIGTERM, failures wrapped as *ExecError.
type cliRunner struct {
	name   string // the tool binary, e.g. "docker"
	logger *slog.Logger
}

// run executes the tool with args and returns its stdout bytes. On context
// cancellation it returns the context error (errors.Is-recoverable); on any
// other failure it returns *ExecError carrying the argv, exit code, and a
// bounded stderr tail.
func (c *cliRunner) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	setupProcess(cmd)
	c.logger.DebugContext(ctx, "exec", "cmd", c.name, "args", args)
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctx.Err() != nil {
		return stdout.Bytes(), fmt.Errorf("infra: %s: %w", c.name, ctx.Err())
	}
	return stdout.Bytes(), &ExecError{
		Cmd:      c.name,
		Args:     args,
		ExitCode: exitCode(err),
		Stderr:   stderrTail(stderr.Bytes()),
		Err:      err,
	}
}

// runStreaming executes the tool with combined stdout+stderr streamed to
// output, returning the exit code. Unlike run, a plain non-zero exit is
// returned as (code, nil) — the docker run verb treats exit codes as data.
func (c *cliRunner) runStreaming(ctx context.Context, output io.Writer, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, c.name, args...)
	cmd.Stdout = output
	cmd.Stderr = output
	setupProcess(cmd)
	c.logger.DebugContext(ctx, "exec", "cmd", c.name, "args", args)
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ctx.Err() != nil {
		return -1, fmt.Errorf("infra: %s: %w", c.name, ctx.Err())
	}
	var xerr *exec.ExitError
	if errors.As(err, &xerr) {
		return xerr.ExitCode(), nil
	}
	return -1, &ExecError{Cmd: c.name, Args: args, ExitCode: -1, Err: err}
}

// exitCode extracts the process exit code from an exec error, or -1 when the
// process never ran.
func exitCode(err error) int {
	var xerr *exec.ExitError
	if errors.As(err, &xerr) {
		return xerr.ExitCode()
	}
	return -1
}

// ensureLogger substitutes a discard logger for nil so adapters never carry
// package-level defaults.
func ensureLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return logger
}
