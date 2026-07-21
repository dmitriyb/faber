// Command faber-box is the in-container phase sequencer: the single
// engine-owned process every step container runs. It is built statically
// (CGO_ENABLED=0 GOOS=linux) from the same module as the host engine,
// bind-mounted read-only into every container, and set as the container
// command — deliberately not image content, so the image stays a pure
// function of the toolset.
//
// Its whole world is the env contract in and the result contract out: it
// reads FABER_* variables, drives the fixed phase order, and guarantees one
// attempt record in the mounted result directory on every exit path.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dmitriyb/faber/agent/box"
)

func main() {
	// The box is normally invoked with no args (env-var driven, as the
	// container's fixed entrypoint); a leading version token is the one
	// exception, checked before anything phase-sequencer-related starts.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			printVersion(os.Stdout)
			return
		}
	}

	// Logs are structured JSON on stderr: the container log is the box's
	// only human channel, and the host asserts phase order from these lines.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	environ := os.Environ()
	b := box.New(box.ParseEnv(environ), box.NewExecRunner(os.Stdout, os.Stderr), environ, logger)
	b.Stdin = os.Stdin // the secrets phase reads the file-mode payload from here
	code := box.Main(ctx, b)
	stop()
	os.Exit(code)
}
