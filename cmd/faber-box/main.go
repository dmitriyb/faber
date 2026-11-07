// Command faber-box is the in-container phase sequencer: the single
// engine-owned process every step container runs. It is built statically
// (CGO_ENABLED=0 GOOS=linux) from the same module as the host engine, baked
// into every template image by `faber build` (its bytes fold into the image
// tag), and set as the container command.
//
// Its whole world is the env contract in and the result contract out: it
// reads FABER_* variables, drives the fixed phase order, and ships one
// attempt record to the host over its standard output on every exit path.
// Its stdout is the frame channel and nothing else; everything human it or
// its children print goes to stderr.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dmitriyb/faber/agent/box"
	"github.com/dmitriyb/faber/agent/contract"
)

func main() {
	// Logs are structured JSON on stderr: the container's stderr is the box's
	// only human channel, and the host asserts phase order from these lines.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	// The box is normally invoked with no args (env-var driven, as the
	// container's fixed entrypoint). The exceptions are checked before
	// anything phase-sequencer-related starts: version, and the two entries
	// of a reconstructed box the operator enters through docker exec.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			printVersion(os.Stdout)
			return
		case "hold":
			os.Exit(box.Hold(ctx))
		case "receive":
			if err := box.Receive(ctx, box.ParseEnv(os.Environ()), os.Stdin, logger); err != nil {
				logger.Error("receive failed", "err", err)
				os.Exit(1)
			}
			return
		}
	}

	environ := os.Environ()
	// Children's stdout joins stderr: fd 1 belongs to the frame channel.
	b := box.New(box.ParseEnv(environ), box.NewExecRunner(os.Stderr, os.Stderr), environ, logger)
	b.Stdin = os.Stdin // the secrets phase reads the file-mode payload from here
	b.Frames = contract.NewFrameWriter(os.Stdout)
	code := box.Main(ctx, b)
	b.Ship(ctx)
	stop()
	os.Exit(code)
}
