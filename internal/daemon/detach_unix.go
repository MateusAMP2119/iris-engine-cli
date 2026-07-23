//go:build unix

package daemon

import "syscall"

// detachedSysProcAttr makes the re-exec'd daemon a session leader (Setsid), so it
// detaches from the CLI's controlling terminal and process group and survives the
// CLI's exit. Unix implementation (darwin + linux); detach_windows.go is the
// console-detachment sibling.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
