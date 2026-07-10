// Package buildinfo carries the build's identity: the version string reported by
// `iris --version`. It is a leaf package (it imports nothing of the product) so
// any layer may read it without inverting the import graph.
package buildinfo

// Version is the build's version string, reported verbatim by `iris --version`.
// It defaults to "dev" for an ordinary (unstamped) build and is set at link time
// with `-ldflags "-X github.com/MateusAMP2119/iris-engine-cli/internal/buildinfo.Version=<tag>"`
// (the release workflow stamps the git tag). This is a build-time constant
// injected by the linker, never mutated at runtime, so despite being a
// package-level var it is not an exception to the no-mutable-package-globals rule:
// nothing in the running program ever writes it.
var Version = "dev"
