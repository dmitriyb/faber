package infra

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// ErrRefAbsent is returned by GitClient.LsRemote when the remote exists but
// has no ref matching the query.
var ErrRefAbsent = errors.New("infra: ref not found on remote")

// lsRemoteAbsentExit is git ls-remote --exit-code's documented status when no
// matching refs are found.
const lsRemoteAbsentExit = 2

// gitCLI is the real GitClient over the git binary. Only plumbing verbs live
// here; their output formats are machine-stable by contract.
type gitCLI struct {
	cli cliRunner
}

// NewGitCLI returns the real git adapter.
func NewGitCLI(logger *slog.Logger) GitClient {
	return &gitCLI{cli: cliRunner{name: "git", logger: ensureLogger(logger).With("adapter", "git")}}
}

func (g *gitCLI) LsRemote(ctx context.Context, url, ref string) (string, error) {
	out, err := g.cli.run(ctx, "ls-remote", "--exit-code", url, ref)
	if err != nil {
		var xerr *ExecError
		if errors.As(err, &xerr) && xerr.ExitCode == lsRemoteAbsentExit {
			return "", fmt.Errorf("infra: git ls-remote %s %s: %w", url, ref, ErrRefAbsent)
		}
		return "", fmt.Errorf("infra: git ls-remote %s %s: %w", url, ref, err)
	}
	sha, perr := parseLsRemote(out)
	if perr != nil {
		return "", fmt.Errorf("infra: git ls-remote %s %s: %w", url, ref, perr)
	}
	return sha, nil
}

// parseLsRemote splits the plumbing output's first line ("<sha>\t<ref>") —
// the one deliberate non-JSON parse in the git adapter, fixed-format and
// handled in exactly this function.
func parseLsRemote(out []byte) (string, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	sha, _, ok := strings.Cut(line, "\t")
	if !ok || !isHexSHA(sha) {
		return "", fmt.Errorf("unexpected plumbing output %q", line)
	}
	return sha, nil
}

// isHexSHA reports whether s is a full sha1 or sha256 object name.
func isHexSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
