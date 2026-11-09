package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// SeedRemote fills a check run's remote volume with the bare repository the
// box's one remote names: the host runs it in a short container of the
// step's image, the volume at env.RemoteDir, a `git bundle` of the working
// directory's repository on stdin. The bundle lands in a file under the
// volume, is cloned bare into a temporary directory beside it, and the
// directory is renamed to <repo>.git — a rename onto an existing, non-empty
// directory fails, so a repository already there is left alone and two
// seeders can never interleave. The tree is chowned to the run user and the
// bundle removed on every path. Success means the repository exists
// afterwards, whoever made it.
func SeedRemote(ctx context.Context, env *BoxEnv, repo string, in io.Reader, runner CmdRunner, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := checkRepoPath(repo); err != nil {
		return fmt.Errorf("seed-remote: repository %q: %w", repo, err)
	}
	bare := filepath.Join(env.RemoteDir, filepath.FromSlash(repo)+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		return fmt.Errorf("seed-remote: %w", err)
	}
	if _, err := os.Stat(bare); err == nil {
		log.InfoContext(ctx, "remote already seeded", "repo", repo)
		return nil
	}
	bundle := filepath.Join(env.RemoteDir, "."+strings.ReplaceAll(repo, "/", "-")+".bundle")
	defer os.Remove(bundle)
	f, err := os.OpenFile(bundle, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("seed-remote: %w", err)
	}
	n, err := io.Copy(f, in)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("seed-remote: read the bundle: %w", err)
	}
	if n == 0 {
		return errors.New("seed-remote: no bundle on stdin")
	}
	tmp, err := os.MkdirTemp(filepath.Dir(bare), "."+filepath.Base(repo)+".tmp-")
	if err != nil {
		return fmt.Errorf("seed-remote: %w", err)
	}
	defer os.RemoveAll(tmp)
	res, err := runner.Run(ctx, CmdSpec{Argv: []string{"git", "clone", "--bare", "--quiet", bundle, tmp}})
	if err != nil {
		return fmt.Errorf("seed-remote: git clone: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("seed-remote: git clone --bare exited %d: %s", res.ExitCode, strings.TrimSpace(string(res.StderrTail)))
	}
	if os.Getuid() == 0 && env.RunUID != 0 {
		if err := chownTree(tmp, env.RunUID, env.RunGID); err != nil {
			return fmt.Errorf("seed-remote: chown: %w", err)
		}
	}
	first, err := installBare(tmp, bare)
	if err != nil {
		return fmt.Errorf("seed-remote: %w", err)
	}
	if first {
		log.InfoContext(ctx, "remote seeded", "repo", repo)
	} else {
		log.InfoContext(ctx, "remote seeded by another", "repo", repo)
	}
	return nil
}

// repoPath is the remote binding's grammar for a repository path: slash
// separated segments of [A-Za-z0-9._-], restated here because the box
// cannot import the security module.
var repoPath = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// checkRepoPath accepts what the remote binding's clone URL accepts and
// nothing that could leave the remote directory.
func checkRepoPath(repo string) error {
	if !repoPath.MatchString(repo) {
		return errors.New("not a plain repository path (segments of [A-Za-z0-9._-] separated by '/')")
	}
	for _, seg := range strings.Split(repo, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("path segment %q is not allowed", seg)
		}
	}
	return nil
}

// installBare moves the freshly cloned tmp into place as bare. MkdirTemp
// made tmp, so the only occupied target is another seeder's or an earlier
// run's repository: that outcome stands, and first reports whether this
// call's clone is the one in place.
func installBare(tmp, bare string) (first bool, err error) {
	if err := os.Rename(tmp, bare); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
