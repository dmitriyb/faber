//go:build linux

package security

import (
	"fmt"
	"syscall"
)

// tmpfsMagic is TMPFS_MAGIC from statfs(2).
const tmpfsMagic = 0x01021994

// isTmpfsDir reports whether path lives on a tmpfs filesystem — the
// precondition for writing a file-mode credential there, so a raw token never
// touches disk.
func isTmpfsDir(path string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false, fmt.Errorf("statfs %s: %w", path, err)
	}
	return st.Type == tmpfsMagic, nil
}
