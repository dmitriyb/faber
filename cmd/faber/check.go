package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmitriyb/faber/config"
	"github.com/dmitriyb/faber/infra"
)

// The committer identity every role uses in a check run: there is no forge
// to verify a signature against, and the registry's real addresses stay out
// of a sandbox. Explicit, never a fallback.
const (
	checkGitName  = "faber-check"
	checkGitEmail = "faber-check@localhost"
)

// checkSandbox is what a check run owns and discards with it: one temporary
// directory holding the journal store and the key every role signs with,
// and the named docker volumes under its own project prefix — the remote
// holding the bare repositories standing in for the gateway, and the
// templates' volumes, which a check never shares with the real project.
type checkSandbox struct {
	dir     string
	project string // the check's own project id: its volumes are faber-<project>-*
	volume  string // the remote's volume
	key     string
	source  string // the working directory's repository; "" when none
	keep    bool
	docker  infra.DockerClient
	stderr  io.Writer
	log     *slog.Logger
}

// newCheckSandbox lays the directory out, names the volume after it and
// generates the key. The working directory's repository is looked up here
// so a repo-bound step can say, before any container, that there is none
// to stand in. The volume itself is the daemon's to create on first use.
func newCheckSandbox(ctx context.Context, docker infra.DockerClient, keep bool, stderr io.Writer, logger *slog.Logger) (*checkSandbox, error) {
	dir, err := os.MkdirTemp("", "faber-check-*")
	if err != nil {
		return nil, fmt.Errorf("faber check: temporary directory: %w", err)
	}
	// The templates' volumes are faber-check-<token>-<name>; the remote is
	// faber-check-remote-<token>, a name no template volume can take, since
	// their second segment is the token's digits.
	token := strings.TrimPrefix(filepath.Base(dir), "faber-check-")
	project := "check-" + token
	sb := &checkSandbox{dir: dir, project: project, volume: "faber-check-remote-" + token, key: filepath.Join(dir, "check_ed25519"), keep: keep, docker: docker, stderr: stderr, log: logger}
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

// close removes the sandbox — the directory and every volume under the
// check's prefix — or keeps them and says where they are.
func (s *checkSandbox) close() {
	if s.keep {
		fmt.Fprintf(s.stderr, "faber check: kept %s and the docker volumes %s and %s*\n", s.dir, s.volume, config.VolumePrefix(s.project))
		return
	}
	if err := os.RemoveAll(s.dir); err != nil {
		s.log.Warn("remove check sandbox", "dir", s.dir, "err", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	names, err := s.docker.VolumeList(ctx, config.VolumePrefix(s.project))
	if err != nil {
		s.log.Warn("list check volumes", "prefix", config.VolumePrefix(s.project), "err", err)
		names = nil
	}
	for _, name := range append(names, s.volume) {
		if err := s.docker.VolumeRemove(ctx, name); err != nil {
			s.log.Warn("remove check volume", "volume", name, "err", err)
		}
	}
}
