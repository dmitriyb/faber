package config

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// isTerminal reports whether w is a terminal. Only an *os.File can be.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// ProgressMode resolves --progress: "auto" (or "") is text on a terminal
// and json otherwise; text, json, and none are explicit.
func ProgressMode(mode string, stderr io.Writer) (string, error) {
	switch mode {
	case "auto", "":
		if isTerminal(stderr) {
			return "text", nil
		}
		return "json", nil
	case "text", "json", "none":
		return mode, nil
	}
	return "", fmt.Errorf("config: unknown --progress %q (auto|text|json|none)", mode)
}

// ColorEnabled resolves --color: "auto" (or "") colours only a terminal
// that is not TERM=dumb and has NO_COLOR unset. TERM and NO_COLOR are the
// terminal's own description — the same class of fact as isatty, read the
// way every terminal program reads it — not faber configuration, which
// stays in explicit files.
func ColorEnabled(mode string, stderr io.Writer) (bool, error) {
	switch mode {
	case "always":
		return true, nil
	case "never":
		return false, nil
	case "auto", "":
		if !isTerminal(stderr) {
			return false, nil
		}
		if os.Getenv("TERM") == "dumb" {
			return false, nil
		}
		if _, set := os.LookupEnv("NO_COLOR"); set {
			return false, nil
		}
		return true, nil
	}
	return false, fmt.Errorf("config: unknown --color %q (auto|always|never)", mode)
}
