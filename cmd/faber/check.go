package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmitriyb/faber/infra"
)

// The committer identity every role uses in a check run: there is no forge
// to verify a signature against, and the registry's real addresses stay out
// of a sandbox. Explicit, never a fallback.
const (
	checkGitName  = "faber-check"
	checkGitEmail = "faber-check@localhost"
)

// checkSandbox is what a check run owns on the host and discards with it:
// one temporary directory holding the journal store, the bare repositories
// standing in for the gateway, and the key every role signs with.
type checkSandbox struct {
	dir       string
	remoteDir string
	key       string
	source    string // the working directory's repository; "" when none
	keep      bool
	stderr    io.Writer
	log       *slog.Logger
}

// newCheckSandbox lays the directory out and generates the key. The
// working directory's repository is looked up here so a repo-bound step
// can say, before any container, that there is none to stand in.
func newCheckSandbox(ctx context.Context, keep bool, stderr io.Writer, logger *slog.Logger) (*checkSandbox, error) {
	dir, err := os.MkdirTemp("", "faber-check-*")
	if err != nil {
		return nil, fmt.Errorf("faber check: temporary directory: %w", err)
	}
	sb := &checkSandbox{dir: dir, remoteDir: filepath.Join(dir, "remote"), key: filepath.Join(dir, "check_ed25519"), keep: keep, stderr: stderr, log: logger}
	if err := os.MkdirAll(sb.remoteDir, 0o755); err != nil {
		sb.close()
		return nil, fmt.Errorf("faber check: %w", err)
	}
	runner := infra.NewCommandRunner(logger)
	res, err := runner.Run(ctx, infra.CmdSpec{Path: "ssh-keygen", Args: []string{"-q", "-t", "ed25519", "-N", "", "-C", checkGitName, "-f", sb.key}})
	if err != nil || res.ExitCode != 0 {
		sb.close()
		if err == nil {
			err = fmt.Errorf("ssh-keygen exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
		return nil, fmt.Errorf("faber check: generate the check key: %w", err)
	}
	if _, err := os.Stat(sb.key); err != nil {
		// ssh-keygen's exit code is not the key: the file is.
		sb.close()
		return nil, fmt.Errorf("faber check: generate the check key: %w", err)
	}
	if res, err := runner.Run(ctx, infra.CmdSpec{Path: "git", Args: []string{"rev-parse", "--show-toplevel"}}); err == nil && res.ExitCode == 0 {
		sb.source = strings.TrimSpace(string(res.Stdout))
	}
	return sb, nil
}

// close removes the sandbox, or keeps it and says where it is.
func (s *checkSandbox) close() {
	if s.keep {
		fmt.Fprintf(s.stderr, "faber check: kept %s\n", s.dir)
		return
	}
	if err := os.RemoveAll(s.dir); err != nil {
		s.log.Warn("remove check sandbox", "dir", s.dir, "err", err)
	}
}
