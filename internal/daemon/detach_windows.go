//go:build windows

package daemon

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedSysProcAttr detaches the re-exec'd daemon from the CLI's console
// (DETACHED_PROCESS) and its Ctrl+C group (CREATE_NEW_PROCESS_GROUP), the
// Windows analogue of the unix Setsid session leader: the daemon survives the
// CLI's exit and the console's interrupts.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
