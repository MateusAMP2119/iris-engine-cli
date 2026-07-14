package daemon

import (
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-engine-cli/internal/config"
)

// This file is the daemon's slog wiring (structured JSON logs (slog); human console
// in foreground). LoggerFor is the pure constructor that picks the handler for a
// run mode -- a JSON handler when the daemon runs detached, a human-readable text
// handler when it runs attached in the foreground. OpenDaemonLogger is the
// daemon-mode path: it routes that JSON logger through the size-based rotator over
// the workspace daemon.log, so a detached daemon's structured logs land in the
// rotated log the operator tails.

// LogMode selects the daemon's log handler.
type LogMode int

const (
	// LogModeForeground is an attached daemon: human-readable text to the console.
	LogModeForeground LogMode = iota
	// LogModeDaemon is a detached/daemonized daemon: structured JSON.
	LogModeDaemon
)

// LoggerFor builds the daemon's slog.Logger for mode, writing to w. In daemon mode
// it emits structured JSON (slog's JSON handler); in foreground mode it emits
// human-readable key=value text (slog's text handler) for a console. It is a pure
// constructor -- no I/O of its own -- so the output shape is asserted directly and
// the lifecycle wiring supplies the sink (a rotator for a daemon, stderr for a
// foreground console).
func LoggerFor(mode LogMode, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if mode == LogModeDaemon {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// OpenDaemonLogger builds the structured JSON logger a detached daemon writes to
// its size-rotated daemon.log, returning the logger and the rotator to close on
// shutdown (JSON logs, size-based rotation). It ensures the log directory first.
// The caller closes the returned io.Closer when the daemon exits.
func OpenDaemonLogger(s config.Settings) (*slog.Logger, io.Closer, error) {
	if err := EnsureLogsDir(s); err != nil {
		return nil, nil, err
	}
	rot, err := NewSizeRotator(LogPath(s), DaemonLogMaxBytes, DaemonLogGenerations)
	if err != nil {
		return nil, nil, err
	}
	return LoggerFor(LogModeDaemon, rot), rot, nil
}
