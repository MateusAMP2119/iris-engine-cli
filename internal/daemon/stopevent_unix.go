//go:build unix

package daemon

// WatchStopEvent is a no-op on unix: a foreground or detached daemon is
// reachable by SIGTERM/SIGINT, which signal.NotifyContext already watches.
// stopevent_windows.go carries the real implementation.
func WatchStopEvent(func()) {}
