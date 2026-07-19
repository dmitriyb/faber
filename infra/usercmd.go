package infra

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

// maxUserCmdStdout bounds a user command's captured stdout. Data-source item
// documents and credential tokens are orders of magnitude smaller; a command
// streaming without bound must fail loudly instead of buffering host memory.
const maxUserCmdStdout = 64 << 20

// errStdoutTooLarge aborts the command when the cap is hit. Deliberately
// value-free: stdout may be a credential.
var errStdoutTooLarge = fmt.Errorf("infra: user command stdout exceeded the %d-byte bound", maxUserCmdStdout)

// cappedBuffer is a bytes.Buffer that refuses writes past max. The refusal
// closes the pipe, which typically kills the producer with SIGPIPE — whose
// exit error would mask the copier's — so Run consults the over flag rather
// than relying on error propagation.
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.over = true
		return 0, errStdoutTooLarge
	}
	return c.buf.Write(p)
}

// userCmd is the real CommandRunner: it executes the user's command file
// directly (no shell interpolation) against a minimal base environment.
// Because stdout may be a credential, neither errors nor log records from
// this adapter ever include stdout bytes or the argument list — stderr and
// the exit code must be sufficient diagnosis.
type userCmd struct {
	logger *slog.Logger
}

// NewCommandRunner returns the real opaque-command adapter.
func NewCommandRunner(logger *slog.Logger) CommandRunner {
	return &userCmd{logger: ensureLogger(logger).With("adapter", "user-command")}
}

func (u *userCmd) Run(ctx context.Context, spec CmdSpec) (CmdResult, error) {
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}
	cmd.Env = append(baseEnv(), spec.Env...)
	stdout := &cappedBuffer{max: maxUserCmdStdout}
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	setupProcess(cmd)

	start := time.Now()
	err := cmd.Run()
	if stdout.over {
		// The bound refusal outranks whatever exit the pipe closure caused
		// (typically SIGPIPE), and the partial bytes are never returned.
		return CmdResult{Stderr: tailBytes(stderr.Bytes()), ExitCode: exitCodeOf(err)},
			fmt.Errorf("infra: user command %s: %w", spec.Path, errStdoutTooLarge)
	}
	res := CmdResult{
		Stdout:   stdout.buf.Bytes(),
		Stderr:   tailBytes(stderr.Bytes()),
		ExitCode: 0,
	}
	// The log record carries the path, exit, and duration — never stdout,
	// never the args.
	u.logger.DebugContext(ctx, "user command finished",
		"path", spec.Path, "exit", exitCodeOf(err), "duration", time.Since(start))
	if err == nil {
		return res, nil
	}
	res.ExitCode = exitCode(err)
	if ctx.Err() != nil {
		return res, fmt.Errorf("infra: user command %s: %w", spec.Path, ctx.Err())
	}
	return res, fmt.Errorf("infra: user command: %w", &ExecError{
		Cmd:      "user-command",
		Args:     []string{spec.Path}, // redacted: never the real argv
		ExitCode: res.ExitCode,
		Stderr:   stderrTail(stderr.Bytes()),
		Err:      err,
	})
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	return exitCode(err)
}

// baseEnv is the minimal environment user commands run against: enough to
// execute tools and write temp files, never the full host environment.
func baseEnv() []string {
	var env []string
	for _, key := range []string{"PATH", "HOME", "TMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}
