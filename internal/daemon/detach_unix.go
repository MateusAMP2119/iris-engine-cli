//go:build unix

package daemon

import "syscall"

// detachedSysProcAttr makes the re-exec'd daemon a session leader (Setsid), so it
// detaches from the CLI's controlling terminal and process group and survives the
// CLI's exit. Unix only (darwin + linux); Windows is deferred from v1
// (specification section 16), matching the exec package.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
