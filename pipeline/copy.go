package pipeline

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// copyTree recursively copies the source tree at src into dst as real,
// world-readable files (directories 0o755, files 0o644); non-regular entries
// are skipped. The interactive re-entry uses it to hand a box its own copy of
// a saved session, which the box must be able to write.
func copyTree(dst, src string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(target, path)
	})
}

// copyFile streams src into a fresh world-readable dst (0o644) rather than
// slurping it whole, so a large file never spikes host memory.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
