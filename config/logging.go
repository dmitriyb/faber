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
// auto-detection). Modules receive child loggers via
// logger.With("component", name) — there is no global logger and nothing calls
// slog.SetDefault. Logs go to stderr; stdout is for program output.
func InitLogging(level, format string, stderr io.Writer) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
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
		if f, ok := stderr.(*os.File); ok {
			useText = term.IsTerminal(int(f.Fd()))
		}
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
