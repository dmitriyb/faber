// Command faber is the workflow engine's CLI entry point. It is deliberately
// minimal: all dispatch lives in the config package so the whole CLI is
// testable in-process; wire.go injects the cross-module capabilities (infra,
// security, agent, metering, failure, pipeline) at integration time.
package main

import (
	"fmt"
	"os"

	"github.com/dmitriyb/faber/config"
)

func main() {
	deps, err := wireDeps(os.Stdout, os.Stderr)
	if err != nil {
		// A malformed host config refuses the whole invocation (including
		// version/help): faber never runs with half-read host state, and
		// there is no ambient fallback to degrade to. Operational exit (1).
		fmt.Fprintln(os.Stderr, "faber:", err)
		os.Exit(1)
	}
	os.Exit(config.RunWithDeps(os.Args[1:], os.Stdout, os.Stderr, deps))
}
