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

// signalStop asks the daemon to shut down gracefully by signalling its named
// stop event (see WatchStopEvent), the Windows stand-in for SIGTERM: the daemon
// drains and stops its managed postmaster cleanly instead of leaving it to
// crash recovery. A daemon without the event (or a failed signal) is killed
// outright; StopDaemon's grace deadline still escalates to Kill either way.
func signalStop(proc *os.Process) error {
	name, err := stopEventName(proc.Pid)
	if err != nil {
		return proc.Kill()
	}
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, name)
	if err != nil {
		return proc.Kill()
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.SetEvent(h); err != nil {
		return proc.Kill()
	}
	return nil
}
