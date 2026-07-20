package metering

import (
	"context"
	"os"
	"testing"
	"time"
)

// Verifies 58879b841ed4 (L-P2d): the probe runner is armed like every other
// opaque subprocess — its own process group, SIGTERM on cancellation, SIGKILL
// escalation — so a hung probe (or its orphaned children holding the stdout
// pipe) cannot block shutdown.
func TestExecRunnerCancelKillsProcessGroup(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// The backgrounded child inherits the stdout pipe: without group
		// cancellation, killing sh alone leaves the pipe open and Output()
		// blocks for the full 30s.
		_, err := ExecRunner{}.Run(ctx, []string{"/bin/sh", "-c", "sleep 30 & sleep 30"}, nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled probe must return an error")
		}
	case <-time.After(probeKillGrace + 5*time.Second):
		t.Fatal("cancelled probe did not return within the kill grace — process group not armed")
	}
}
