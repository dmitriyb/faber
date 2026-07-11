package infra

import (
	"fmt"
	"strings"
)

// stderrTailLimit bounds how much stderr an ExecError (or CmdResult) retains:
// enough to debug without re-running, small enough to embed in a record.
const stderrTailLimit = 4 << 10

// ExecError is the typed failure of one external tool invocation. Every
// non-zero exit or spawn failure from an adapter surfaces as *ExecError
// (wrapped with the operation context), so callers recover exit codes with
// errors.As. For user commands Args is redacted to the command path only and
// stdout never appears anywhere — resolver output is a potential credential.
type ExecError struct {
	Cmd      string   // "docker", "git", "nix", or "user-command"
	Args     []string // redacted to path-only for user commands
	ExitCode int      // -1 when the process never ran or was killed
	Stderr   string   // trimmed tail, bounded (4 KiB)
	Err      error    // underlying exec error, %w-wrapped
}

func (e *ExecError) Error() string {
	var b strings.Builder
	b.WriteString(e.Cmd)
	if len(e.Args) > 0 {
		fmt.Fprintf(&b, " %s", strings.Join(e.Args, " "))
	}
	if e.ExitCode >= 0 {
		fmt.Fprintf(&b, ": exit %d", e.ExitCode)
	} else if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if e.Stderr != "" {
		fmt.Fprintf(&b, ": %s", e.Stderr)
	}
	return b.String()
}

func (e *ExecError) Unwrap() error { return e.Err }

// stderrTail trims and bounds captured stderr to its last stderrTailLimit
// bytes.
func stderrTail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > stderrTailLimit {
		s = s[len(s)-stderrTailLimit:]
	}
	return s
}

// tailBytes bounds a byte slice to its last stderrTailLimit bytes.
func tailBytes(b []byte) []byte {
	if len(b) > stderrTailLimit {
		b = b[len(b)-stderrTailLimit:]
	}
	return b
}
