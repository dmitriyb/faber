package main

import (
	"fmt"
	"io"
)

// version, commit, and date are stamped by GoReleaser via -ldflags at build
// time, identically to cmd/faber's own build-info vars — the two binaries
// ship from the same tag but are separate `main` packages, so each carries
// its own copy. A locally built binary keeps these defaults.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func printVersion(w io.Writer) {
	fmt.Fprintf(w, "faber-box %s (commit %s, built %s)\n", version, commit, date)
}
