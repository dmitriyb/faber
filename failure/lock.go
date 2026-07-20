package failure

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// runLockFile is the advisory run lock's name inside a run directory.
const runLockFile = "run.lock"

// RunLock is the run directory's advisory, process-lifetime exclusivity lock:
// a flock(2) on <runDir>/run.lock. It guards the journal's single-appender
// invariant — exactly one process may hold a run open for appends (fresh or
// resume) — so torn-tail repair can never truncate a line another live
// process just committed and two processes can never adopt each other's
// attempt directories. The kernel releases the flock when the holder exits,
// however it exits, so there is no stale-lock state to recover from.
type RunLock struct {
	f *os.File
}

// AcquireRunLock takes runDir's advisory lock without blocking. A held lock
// is a loud, structured refusal naming the recorded holder pid. The file's
// pid content is diagnostics only — liveness is the flock itself.
func AcquireRunLock(runDir string) (*RunLock, error) {
	path := filepath.Join(runDir, runLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failure: open run lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := lockHolder(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf(
				"failure: run %s is live: another faber process (pid %s) holds %s; a run accepts one process at a time — wait for it or stop it first",
				filepath.Base(runDir), holder, path)
		}
		return nil, fmt.Errorf("failure: lock run %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	return &RunLock{f: f}, nil
}

// Release drops the lock by closing the file. The lock file itself stays
// behind deliberately: unlinking a flocked path lets a third process lock a
// fresh inode while a second still waits on the old one, and a leftover file
// grants nothing — only a live flock does.
func (l *RunLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	if err != nil {
		return fmt.Errorf("failure: release run lock: %w", err)
	}
	return nil
}

// RunLive reports whether some process currently holds runDir's lock. It
// probes with a non-blocking flock and releases immediately on success, so it
// never disturbs a legitimate holder and never retains the lock itself. A
// missing lock file (or missing run dir) means no holder. The probe window is
// real but fail-safe: an acquirer racing the probe can see a spurious
// EWOULDBLOCK and refuse — the advisory answer here is a pre-flight signal
// (the upgrade guard), never a correctness gate.
func RunLive(runDir string) bool {
	f, err := os.Open(filepath.Join(runDir, runLockFile))
	if err != nil {
		return false
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

// lockHolder reads the pid a live holder recorded, for refusal messages.
func lockHolder(f *os.File) string {
	buf := make([]byte, 32)
	n, err := f.ReadAt(buf, 0)
	if n == 0 && err != nil {
		return "unknown"
	}
	pid := string(bytes.TrimSpace(buf[:n]))
	if pid == "" {
		return "unknown"
	}
	return pid
}
