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
	// WaitDelay's SIGKILL reaches only the direct child; a SIGTERM-ignoring
	// grandchild survives it, but the force-closed pipes still unblock Wait,
	// so shutdown completes regardless.
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
// When stdin is non-nil os/exec copies it to the child's standard input and
// closes the pipe on EOF; the bytes are never logged (they may be a
// credential). stdin == nil leaves the child's stdin unattached.
//
// The EOF/close guarantee is load-bearing: it is what lets faber-box's
// io.ReadAll(stdin) return instead of blocking forever. It holds because the
// caller (ContainerRunner.Run) passes a *bytes.Reader — a non-*os.File reader.
// For such a reader os/exec creates an os.Pipe, spawns a copier goroutine, and
// closes the pipe's write end when the copy hits EOF, so the container sees a
// clean end-of-payload. Were cmd.Stdin ever an *os.File (or an already-open fd)
// os/exec would dup that fd straight through with no copier and no close-on-EOF,
// and the box's read would hang — do not pass an *os.File here.
func (c *cliRunner) runStreaming(ctx context.Context, output io.Writer, stdin io.Reader, args ...string) (int, error) {
	cmd := exec.CommandContext(ctx, c.name, args...)
	cmd.Stdout = output
	cmd.Stderr = output
	if stdin != nil {
		cmd.Stdin = stdin
	}
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
