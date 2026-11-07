package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmitriyb/faber/agent/contract"
)

// Hold is the entry of a reconstructed box the operator enters through
// docker exec: it keeps the container alive until a termination signal
// cancels the context, and does nothing else.
func Hold(ctx context.Context) int {
	<-ctx.Done()
	return 0
}

// Receive reads a frame stream from in and writes it into the container:
// handoff/<rel> under the handoff directory and sessions/<rel> under the
// sessions directory, refusing any other first segment and sessions/ when
// no sessions directory is set. Files arrive through temporary names with
// the frames' mtimes, links are recreated, every write goes through an
// os.Root of its tree, and both trees are then chowned to the run user, so
// the operator's exec — which drops to that user — owns what it finds. A
// held box ran no preamble, so what the preamble would have chowned is
// chowned here too: the workspace, /tmp, HOME, and the path from HOME down
// to the sessions directory. It runs as root through docker exec, before
// that exec.
func Receive(ctx context.Context, env *BoxEnv, in io.Reader, log *slog.Logger) error {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	r := &receiver{env: env, roots: map[string]*os.Root{}}
	defer r.close()
	if err := contract.ReadFrames(in, r); err != nil {
		return fmt.Errorf("receive: %w", err)
	}
	if os.Getuid() != 0 || env.RunUID == 0 {
		return nil
	}
	for _, dir := range r.touched() {
		if err := chownTree(dir, env.RunUID, env.RunGID); err != nil {
			return fmt.Errorf("receive: chown %s: %w", dir, err)
		}
	}
	for _, p := range holdChownPaths(env) {
		if err := os.Lchown(p, env.RunUID, env.RunGID); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("receive: chown %s: %w", p, err)
		}
	}
	log.InfoContext(ctx, "received", "files", r.files, "links", r.links)
	return nil
}

// holdChownPaths is what the preamble chowns for a sequenced box and a held
// box lacks: the writable mounts (non-recursive) and the components between
// HOME and the sessions directory, exclusive of HOME itself, over the
// cleaned value — the walk runs as root, so a value outside HOME contributes
// nothing.
func holdChownPaths(env *BoxEnv) []string {
	out := []string{env.WorkspaceDir, "/tmp", contract.ContainerHome}
	if env.SessionsDir != "" {
		for p := filepath.Dir(filepath.Clean(env.SessionsDir)); strings.HasPrefix(p, contract.ContainerHome+"/"); p = filepath.Dir(p) {
			out = append(out, p)
		}
	}
	return out
}

// receiver is the frame handler behind Receive.
type receiver struct {
	env          *BoxEnv
	roots        map[string]*os.Root
	files, links int
}

// target maps a frame name onto the root of its tree and the path beneath
// it, or refuses the name.
func (r *receiver) target(name string) (*os.Root, string, error) {
	rootName, rel, ok := strings.Cut(name, "/")
	if !ok {
		return nil, "", fmt.Errorf("frame %q names no file under a root", name)
	}
	var dir string
	switch rootName {
	case "handoff":
		dir = r.env.HandoffDir
	case "sessions":
		if r.env.SessionsDir == "" {
			return nil, "", fmt.Errorf("frame %q: no sessions directory on this box (%s unset)", name, contract.EnvSessionsDir)
		}
		dir = r.env.SessionsDir
	default:
		return nil, "", fmt.Errorf("frame %q: root %q is neither handoff nor sessions", name, rootName)
	}
	root, ok := r.roots[rootName]
	if !ok {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, "", err
		}
		var err error
		if root, err = os.OpenRoot(dir); err != nil {
			return nil, "", err
		}
		r.roots[rootName] = root
	}
	return root, rel, nil
}

func (r *receiver) Frame(kind, name string, mtime time.Time, body io.Reader, n int64) error {
	switch kind {
	case contract.FrameEnd:
		return nil
	case contract.FrameFile:
		root, rel, err := r.target(name)
		if err != nil {
			return err
		}
		r.files++
		return contract.WriteFrameFile(root, rel, body, n, mtime)
	case contract.FrameLink:
		if !strings.HasPrefix(name, "sessions/") {
			return fmt.Errorf("frame link %q: links are received under sessions/ only", name)
		}
		root, rel, err := r.target(name)
		if err != nil {
			return err
		}
		r.links++
		return contract.WriteFrameLink(root, rel, body, n)
	}
	return fmt.Errorf("frame %s %q: only files and links are received", kind, name)
}

func (r *receiver) Foreign(p []byte) error {
	return errors.New("bytes outside any frame on the receive stream")
}

// touched lists the trees the stream wrote under.
func (r *receiver) touched() []string {
	var out []string
	if _, ok := r.roots["handoff"]; ok {
		out = append(out, r.env.HandoffDir)
	}
	if _, ok := r.roots["sessions"]; ok {
		out = append(out, r.env.SessionsDir)
	}
	return out
}

func (r *receiver) close() {
	for _, root := range r.roots {
		root.Close()
	}
}

// chownTree gives root and everything under it to uid:gid, links included
// (never followed: a link's target may be outside the tree).
func chownTree(root string, uid, gid int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}
