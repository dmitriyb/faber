package box

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// stderrTailCap bounds the stderr tail retained for handoff records.
const stderrTailCap = 4 << 10

// runnerKillGrace is how long a cancelled subprocess gets between SIGTERM to
// its process group and SIGKILL escalation.
const runnerKillGrace = 10 * time.Second

// CmdSpec is one subprocess invocation inside the box: an explicit argv
// (never a shell), a working directory, and the exact child environment.
type CmdSpec struct {
	Argv []string
	Dir  string
	Env  []string
}

// CmdResult is a finished invocation. A non-zero ExitCode is data, not an
// error — the phases decide what an exit code means. StderrTail is the
// bounded last bytes of stderr, kept for handoff records; because a hook may
// echo delegated values, it is never logged, only recorded where the spec
// directs.
type CmdResult struct {
	Stdout     []byte
	StderrTail []byte
	ExitCode   int
}

// CmdRunner is the box's single subprocess seam (git, ssh-add, hooks, the
// agent CLI); unit tests substitute a fake and never exec anything.
type CmdRunner interface {
	// Run executes with captured stdout (for the engine to type) and stderr
	// streamed to the box log plus retained as a tail.
	Run(ctx context.Context, spec CmdSpec) (CmdResult, error)

	// Stream executes with stdout inherited by the container log (never
	// parsed) and stderr streamed plus retained as a tail. Used for the
	// user-filled phases: hooks and the agent invocation.
	Stream(ctx context.Context, spec CmdSpec) (CmdResult, error)
}

// execRunner is the real CmdRunner over os/exec.
type execRunner struct {
	stdout io.Writer
	stderr io.Writer
}

// NewExecRunner constructs the real runner; stdout/stderr are the container's
// log streams (os.Stdout/os.Stderr in the faber-box binary).
func NewExecRunner(stdout, stderr io.Writer) CmdRunner {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &execRunner{stdout: stdout, stderr: stderr}
}

func (r *execRunner) Run(ctx context.Context, spec CmdSpec) (CmdResult, error) {
	var stdout bytes.Buffer
	return r.exec(ctx, spec, &stdout)
}

func (r *execRunner) Stream(ctx context.Context, spec CmdSpec) (CmdResult, error) {
	return r.exec(ctx, spec, r.stdout)
}

// exec runs the argv with stderr teed to the log and a bounded tail buffer.
func (r *execRunner) exec(ctx context.Context, spec CmdSpec, stdout io.Writer) (CmdResult, error) {
	if len(spec.Argv) == 0 {
		return CmdResult{ExitCode: -1}, errors.New("box: empty argv")
	}
	tail := newTailBuffer(stderrTailCap)
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = stdout
	cmd.Stderr = io.MultiWriter(r.stderr, tail)
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
	cmd.WaitDelay = runnerKillGrace

	err := cmd.Run()
	res := CmdResult{StderrTail: tail.Bytes()}
	if buf, ok := stdout.(*bytes.Buffer); ok {
		res.Stdout = buf.Bytes()
	}
	if err == nil {
		return res, nil
	}
	if ctx.Err() != nil {
		res.ExitCode = -1
		return res, fmt.Errorf("box: %s: %w", spec.Argv[0], ctx.Err())
	}
	var xerr *exec.ExitError
	if errors.As(err, &xerr) {
		res.ExitCode = xerr.ExitCode()
		return res, nil // exit codes are data
	}
	res.ExitCode = -1
	return res, fmt.Errorf("box: %s: %w", spec.Argv[0], err)
}

// tailBuffer is a bounded writer keeping the last max bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTailBuffer(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= t.max {
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		return n, nil
	}
	if keep := t.max - n; len(t.buf) > keep {
		copy(t.buf, t.buf[len(t.buf)-keep:])
		t.buf = t.buf[:keep]
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.buf)
}
