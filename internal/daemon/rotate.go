package daemon

import (
	"errors"
	"fmt"
	"os"
	"sync"
)

// This file is the daemon log's size-based rotator (specification section 2:
// "Size-based rotation only, never time-based: daemon log 10 MB, 5 generations").
// It is a hand-rolled io.WriteCloser -- no rotation dependency is on the Iris
// allowlist -- that checks the active file's size before each write and, when a
// write would cross the threshold, rolls the current file to <name>.1, shifting
// the older generations up and dropping the oldest. Rotation is decided solely by
// size: there is deliberately no timer, ticker, or clock read anywhere here, so a
// time-based rotation cannot creep in.

// The production daemon-log rotation constants (specification section 2). The
// rotator itself is parameterized so tests can force many rotations with a tiny
// threshold; these are the values the daemon wires in.
const (
	// DaemonLogMaxBytes is the size the daemon log rotates at: 10 MB.
	DaemonLogMaxBytes int64 = 10 * 1024 * 1024
	// DaemonLogGenerations is the number of rotated generations kept (daemon.log.1
	// .. daemon.log.5); the oldest is dropped on each rotation.
	DaemonLogGenerations = 5
)

// SizeRotator is a concurrency-safe, size-based rotating writer for the daemon
// log. It appends to a single active file until a write would cross maxBytes, at
// which point it rolls the active file to <path>.1 (shifting <path>.1 -> <path>.2
// and so on, dropping <path>.<generations>) and continues on a fresh active file.
// Rotation is triggered only by accumulated size; nothing here consults a clock,
// so there is no time-based rotation.
type SizeRotator struct {
	mu          sync.Mutex
	path        string
	maxBytes    int64
	generations int
	perm        os.FileMode
	file        *os.File
	size        int64
	// rename performs a generation rename; it defaults to os.Rename and is a seam
	// tests override to inject a transient rotation failure, so the recovery path
	// (never panic on the next write after a failed rotation) is exercised
	// deterministically.
	rename func(oldpath, newpath string) error
}

// NewSizeRotator opens (creating/appending) the active log at path and returns a
// rotator that rolls it at maxBytes, keeping generations rotated backups. maxBytes
// must be positive and generations non-negative. The caller closes the rotator.
func NewSizeRotator(path string, maxBytes int64, generations int) (*SizeRotator, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("daemon: rotator max bytes must be positive, got %d", maxBytes)
	}
	if generations < 0 {
		return nil, fmt.Errorf("daemon: rotator generations must be non-negative, got %d", generations)
	}
	r := &SizeRotator{path: path, maxBytes: maxBytes, generations: generations, perm: logFilePerm, rename: os.Rename}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

// open opens the active file for append and records its current size, so a rotator
// resumed over an existing log rotates from the right offset.
func (r *SizeRotator) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, r.perm) //nolint:gosec // G304: path is the engine-owned daemon log under the workspace .iris tree, not user or network input.
	if err != nil {
		return fmt.Errorf("daemon: open rotating log %s: %w", r.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("daemon: stat rotating log %s: %w", r.path, err)
	}
	r.file = f
	r.size = info.Size()
	return nil
}

// Write appends p to the active file, rotating first when the active file is
// non-empty and appending p would cross the size threshold. It is safe for
// concurrent callers. A single write larger than the threshold lands whole on a
// fresh file rather than being split.
func (r *SizeRotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A prior rotation may have failed partway (a transient disk error on a rename
	// or reopen), leaving the active file closed. Reopen it before use so a write
	// never dereferences a nil file; a still-failing reopen is returned as an
	// error, never panicked.
	if err := r.ensureOpen(); err != nil {
		return 0, err
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			// rotate closed the active file and left it nil after a transient
			// failure; the next Write reopens it via ensureOpen. Surface the error
			// rather than write to a nil file.
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}

// ensureOpen opens the active file when a prior rotation left it closed, so the
// rotator recovers on the next write rather than dereferencing a nil file. The
// caller holds r.mu.
func (r *SizeRotator) ensureOpen() error {
	if r.file != nil {
		return nil
	}
	return r.open()
}

// rotate rolls the active file to <path>.1, shifting each older generation up by
// one and dropping the oldest, then reopens a fresh active file. The caller holds
// r.mu.
func (r *SizeRotator) rotate() error {
	// Close and release the active file. Guard the close so a rotate re-entered
	// after a prior failure (active file already nil) never dereferences nil; the
	// active file is nulled only once it is closed, so a failure below leaves the
	// rotator recoverable (the next Write reopens).
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			return fmt.Errorf("daemon: close log before rotation %s: %w", r.path, err)
		}
		r.file = nil
	}

	if r.generations < 1 {
		// No generations kept: the active file is simply replaced.
		if err := removeIfPresent(r.path); err != nil {
			return err
		}
		return r.open()
	}

	// Drop the oldest generation, then shift the rest up by one (highest first so
	// no rename clobbers a generation still to be moved).
	if err := removeIfPresent(fmt.Sprintf("%s.%d", r.path, r.generations)); err != nil {
		return err
	}
	for g := r.generations - 1; g >= 1; g-- {
		if err := r.renameIfPresent(fmt.Sprintf("%s.%d", r.path, g), fmt.Sprintf("%s.%d", r.path, g+1)); err != nil {
			return err
		}
	}
	if err := r.renameIfPresent(r.path, r.path+".1"); err != nil {
		return err
	}
	return r.open()
}

// Close closes the active file. It is idempotent.
func (r *SizeRotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	if err != nil {
		return fmt.Errorf("daemon: close rotating log %s: %w", r.path, err)
	}
	return nil
}

// removeIfPresent removes path, treating an already-absent path as success.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: drop rotated log %s: %w", path, err)
	}
	return nil
}

// renameIfPresent renames from to to through the rotator's rename seam, treating
// an absent source as success (a generation that has never been written yet).
func (r *SizeRotator) renameIfPresent(from, to string) error {
	if err := r.rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("daemon: rotate log %s -> %s: %w", from, to, err)
	}
	return nil
}
