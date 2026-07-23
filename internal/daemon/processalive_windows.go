//go:build windows

package daemon

import (
	"os"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports for a process that
// has not yet terminated (STILL_ACTIVE).
const stillActive = 259

// processAlive queries the process's exit code: signals cannot probe liveness
// on Windows (Signal(0) fails for every process). An open that fails with
// access-denied still proves the pid names a live process we cannot inspect.
// See the contract comment in lifecycle.go.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// signalStop stops the daemon process on Windows. There is no cross-console
// SIGTERM equivalent for a detached process, so stop is a hard kill; the wait
// loop in StopDaemon still confirms the process is gone before the pidfile is
// reaped. Graceful drain on Windows is deferred to a control-plane shutdown.
func signalStop(proc *os.Process) error {
	return proc.Kill()
}
