// Command iris is the Iris engine and CLI: one binary that is both the
// control-plane daemon and the operator CLI.
//
// This is a deliberately minimal placeholder for epic E00. It prints a version
// string and exits 0 on bare invocation, and exits 2 -- the specification's
// usage-error category (spec section 8) -- on any argument, because no commands
// are wired yet. The full cobra command tree arrives in E01 and the daemon and
// managed Postgres in E02, both of which replace this file wholesale. It is kept
// tiny on purpose: E00's conformance runner needs a real, buildable binary to
// drive, and nothing more.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is the placeholder engine version reported on bare invocation. E01's
// version command supersedes it.
const version = "0.0.0-dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main's testable core: it maps args to an exit code, writing the version
// to out on bare invocation (exit 0) and a usage error to errOut for any
// argument (exit 2), since the command tree does not exist yet.
func run(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(out, "iris %s\n", version)
		return 0
	}
	fmt.Fprintf(errOut, "iris: unknown command %q\n", args[0])
	fmt.Fprintln(errOut, "usage: iris (no commands are wired yet; the command tree arrives in E01)")
	return 2
}
