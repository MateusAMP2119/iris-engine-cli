//go:build windows

package daemon

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// stopEventName returns the session-local named event `engine stop` signals to
// ask the daemon with the given pid for a graceful shutdown. Windows has no
// cross-console SIGTERM, so a kernel event object is the graceful channel; the
// name is derivable from the pidfile alone.
func stopEventName(pid int) (*uint16, error) {
	return windows.UTF16PtrFromString(fmt.Sprintf(`Local\iris-engine-stop-%d`, pid))
}

// WatchStopEvent creates this process's stop event and arms a watcher that
// calls stop when it is signalled, feeding the same cancellation path as
// SIGTERM/SIGINT. A failure to create the event leaves only the hard-kill
// escalation, which StopDaemon still performs; the daemon runs on regardless.
// The event handle is held for the process's lifetime.
func WatchStopEvent(stop func()) {
	name, err := stopEventName(os.Getpid())
	if err != nil {
		return
	}
	h, err := windows.CreateEvent(nil, 1 /* manual reset */, 0 /* unset */, name)
	if err != nil {
		return
	}
	go func() {
		if ev, err := windows.WaitForSingleObject(h, windows.INFINITE); err == nil && ev == windows.WAIT_OBJECT_0 {
			stop()
		}
	}()
}
