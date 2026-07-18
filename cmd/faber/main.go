// Command faber is the workflow engine's CLI entry point. It is deliberately
// minimal: all dispatch lives in the config package so the whole CLI is
// testable in-process; wire.go injects the cross-module capabilities (infra,
// security, agent, metering, failure, pipeline) at integration time.
package main

import (
	"os"

	"github.com/dmitriyb/faber/config"
)

func main() {
	os.Exit(config.RunWithDeps(os.Args[1:], os.Stdout, os.Stderr, wireDeps(os.Stdout, os.Stderr)))
}
