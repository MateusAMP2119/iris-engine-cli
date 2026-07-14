// Command iris is the Iris engine and CLI: one binary that is both the
// control-plane daemon and the operator CLI.
//
// This entrypoint is deliberately thin. It builds nothing itself: the whole
// command surface -- the cobra noun-verb tree, the global flags, and the
// exit-code and --json output contracts -- lives in internal/cli. main passes
// the process arguments and streams to cli.Execute and exits with the exit-code
// category it returns.
package main

import (
	"os"

	"github.com/MateusAMP2119/iris-engine-cli/internal/cli"
)

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
