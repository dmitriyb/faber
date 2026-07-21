package config

import (
	"fmt"
	"io"
)

// BuildInfo carries the version/commit/date the release pipeline stamps into
// cmd/faber's package-main vars via -ldflags. A locally built binary passes a
// zero-valued BuildInfo, and cmdVersion substitutes dev/none/unknown so the
// version command always prints something, never an empty field.
type BuildInfo struct {
	Version string
	Commit  string
	Date    string
}

func cmdVersion(stdout io.Writer, info BuildInfo) int {
	fmt.Fprintf(stdout, "faber %s (commit %s, built %s)\n",
		orDefault(info.Version, "dev"), orDefault(info.Commit, "none"), orDefault(info.Date, "unknown"))
	return 0
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
