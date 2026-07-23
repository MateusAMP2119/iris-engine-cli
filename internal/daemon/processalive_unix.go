//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// processAlive probes pid with the null signal: signal 0 delivers nothing but
// validates the target exists. See the contract comment in lifecycle.go.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// signalStop asks the process to shut down gracefully with SIGTERM. A process
// that is already gone reports os.ErrProcessDone through the returned error.
func signalStop(proc *os.Process) error {
	return proc.Signal(syscall.SIGTERM)
}
