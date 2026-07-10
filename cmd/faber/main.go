// Command faber is the workflow engine's CLI entry point. It is deliberately
// minimal: all dispatch lives in the config package so the whole CLI is
// testable in-process, and cross-module capabilities (infra, pipeline,
// failure) are injected here at integration time.
package main

import (
	"os"

	"github.com/dmitriyb/faber/config"
)

func main() {
	os.Exit(config.Run(os.Args[1:], os.Stdout, os.Stderr))
}
