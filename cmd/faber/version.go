package main

// version, commit, and date are stamped by GoReleaser via -ldflags at build
// time (main.version=..., main.commit=..., main.date=...); a locally built
// binary keeps these defaults, and config.cmdVersion substitutes its own
// dev/none/unknown fallback if these were ever passed through empty.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)
