package config

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"golang.org/x/term"
)

// InitLogging builds the root slog logger: JSON handler when stderr is not a
// TTY, text handler when it is, overridable via --log-format (explicit beats
// auto-detection). An empty level is the auto default: warn on a terminal —
// where the progress stream is the operator surface and slog is diagnostics
// only — and info otherwise, where the JSON handler feeds machines. Modules
// receive child loggers via logger.With("component", name) — there is no
// global logger and nothing calls slog.SetDefault. Logs go to stderr; stdout
// is for program output.
func InitLogging(level, format string, stderr io.Writer) (*slog.Logger, error) {
	tty := false
	if f, ok := stderr.(*os.File); ok {
		tty = term.IsTerminal(int(f.Fd()))
	}
	var lvl slog.Level
	switch level {
	case "":
		lvl = defaultLevel(tty)
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("config: unknown --log-level %q (debug|info|warn|error)", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var useText bool
	switch format {
	case "text":
		useText = true
	case "json":
		useText = false
	case "auto", "":
		useText = tty
	default:
		return nil, fmt.Errorf("config: unknown --log-format %q (auto|json|text)", format)
	}
	var h slog.Handler
	if useText {
		h = slog.NewTextHandler(stderr, opts)
	} else {
		h = slog.NewJSONHandler(stderr, opts)
	}
	return slog.New(h), nil
}

// defaultLevel is the auto log level: diagnostics recede to warn on a
// terminal, where the progress stream carries the heartbeat; off a terminal
// info stays, since there is no operator watching and the JSON lines are the
// record.
func defaultLevel(tty bool) slog.Level {
	if tty {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}
