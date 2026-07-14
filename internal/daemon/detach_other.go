//go:build !unix

package daemon

import "syscall"

// detachedSysProcAttr has no session-leader detachment on non-unix platforms. The
// daemon lifecycle targets unix (darwin + linux); Windows is deferred from v1. This
// stub keeps the package building elsewhere.
func detachedSysProcAttr() *syscall.SysProcAttr { return nil }
