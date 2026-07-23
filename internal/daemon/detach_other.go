//go:build !unix && !windows

package daemon

import "syscall"

// detachedSysProcAttr has no session-leader detachment on platforms without one
// (unix and windows have real implementations). This stub keeps the package
// building elsewhere.
func detachedSysProcAttr() *syscall.SysProcAttr { return nil }
