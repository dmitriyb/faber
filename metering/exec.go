package metering

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

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
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("metering: run %s: %w: %s", argv[0], err, msg)
		}
		return nil, fmt.Errorf("metering: run %s: %w", argv[0], err)
	}
	return out, nil
}
