package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// This file is the multi-process stop shared by `iris engine stop`,
// `iris engine start` (replace prior engines), and `iris uninstall`: every
// local `iris engine start` process is signalled (pidfile + orphans), not only
// the single pid recorded in iris.pid. A leftover managed Postgres is then
// stopped best-effort so a later start or uninstall can bind/remove cleanly.

// listLocalEnginePIDs is the process-table seam StopAllEngines uses.
var listLocalEnginePIDs = listLocalEnginePIDsViaPS

// StopAllEngines stops every local iris engine process: the pidfile daemon and
// any orphan `iris engine start` processes found in the process table. Each
// receives SIGTERM, then SIGKILL if it outruns the grace deadline on ctx. The
// pidfile is reaped when done. A leftover managed Postgres (postmaster still
// alive after the engines exit) is stopped best-effort. It returns the distinct
// PIDs it signalled (alive at the start); an empty slice means nothing was
// running.
func StopAllEngines(ctx context.Context, s config.Settings) ([]int, error) {
	seen := map[int]struct{}{}
	var pids []int
	add := func(pid int) {
		if pid <= 0 || pid == os.Getpid() {
			return
		}
		if _, ok := seen[pid]; ok {
			return
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}

	if listed, err := listLocalEnginePIDs(); err == nil {
		for _, pid := range listed {
			add(pid)
		}
	} else if pidfilePID, perr := ReadPIDFile(s); perr == nil {
		// Process table unavailable: still stop the recorded detached daemon.
		add(pidfilePID)
	} else {
		// No list and no pidfile: still try managed Postgres cleanup below.
	}
	if pidfilePID, err := ReadPIDFile(s); err == nil {
		add(pidfilePID)
	}

	// Only signal processes that are still alive; drop already-gone PIDs from the
	// reported set so "stopped N" means real work.
	var live []int
	for _, pid := range pids {
		if processAlive(pid) {
			live = append(live, pid)
		}
	}
	if err := signalAll(ctx, live); err != nil {
		_ = RemovePIDFile(s)
		return live, err
	}
	_ = RemovePIDFile(s) // engines are down; a sticky pidfile must not fail uninstall
	stopManagedPostgresBestEffort(s)
	return live, nil
}

// signalAll asks every pid to stop gracefully (SIGTERM / Windows stop event via
// signalStop), waits until all are gone or ctx is done, then Kill any still
// alive and confirms they exit.
func signalAll(ctx context.Context, pids []int) error {
	if len(pids) == 0 {
		return nil
	}
	for _, pid := range pids {
		proc, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if err := signalStop(proc); err != nil && !errors.Is(err, os.ErrProcessDone) {
			// Process gone mid-signal: treat as already stopped.
			if !processAlive(pid) {
				continue
			}
			return fmt.Errorf("daemon: signal process %d: %w", pid, err)
		}
	}

	backoff := 10 * time.Millisecond
	for {
		if allGone(pids) {
			return nil
		}
		select {
		case <-ctx.Done():
			// Grace elapsed: force every remaining process down. Matches
			// StopDaemon — Kill is fire-and-forget; a short liveness poll
			// follows, then we return success even if a zombie still briefly
			// answers processAlive (reaped by init for detached engines).
			for _, pid := range pids {
				if !processAlive(pid) {
					continue
				}
				proc, err := os.FindProcess(pid)
				if err != nil {
					continue
				}
				_ = proc.Kill()
			}
			killCtx, cancel := context.WithTimeout(context.Background(), killConfirmTimeout)
			waitAllGone(killCtx, pids)
			cancel()
			return nil
		case <-time.After(backoff):
		}
		if backoff < detachReadyPollBackoff {
			backoff *= 2
		}
	}
}

func allGone(pids []int) bool {
	for _, pid := range pids {
		if processAlive(pid) {
			return false
		}
	}
	return true
}

func waitAllGone(ctx context.Context, pids []int) {
	backoff := 10 * time.Millisecond
	for {
		if allGone(pids) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < detachReadyPollBackoff {
			backoff *= 2
		}
	}
}

// listLocalEnginePIDsViaPS scans the process table for iris engine start
// processes belonging to this host view (ps -A).
func listLocalEnginePIDsViaPS() ([]int, error) {
	// -A: all users' processes with a TTY or not; -o pid=,args= omits headers.
	// Portable across macOS and Linux procps.
	cmd := osexec.Command("ps", "-A", "-o", "pid=,args=")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("daemon: list processes: %w", err)
	}
	return parseEnginePIDsFromPS(out), nil
}

// parseEnginePIDsFromPS extracts iris engine start PIDs from `ps -o pid=,args=`
// output.
func parseEnginePIDsFromPS(out []byte) []int {
	var pids []int
	self := os.Getpid()
	for _, line := range bytes.Split(out, []byte{'\n'}) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// pid is the first field; args is the remainder (may contain spaces).
		fields := strings.Fields(string(line))
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		// Rebuild from fields[1:] so leading spaces after the pid column are gone.
		cmdline := strings.Join(fields[1:], " ")
		if !looksLikeIrisEngineStart(cmdline) {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// looksLikeIrisEngineStart reports whether a process command line is an iris
// engine start invocation: executable basename "iris" and consecutive argv
// tokens "engine" "start" (global flags may sit between the binary and engine).
func looksLikeIrisEngineStart(cmdline string) bool {
	fields := strings.Fields(cmdline)
	if len(fields) < 3 {
		return false
	}
	if filepath.Base(fields[0]) != "iris" {
		return false
	}
	for i := 1; i < len(fields)-1; i++ {
		if fields[i] == "engine" && fields[i+1] == "start" {
			return true
		}
	}
	return false
}

// stopManagedPostgresBestEffort stops a managed postmaster still holding the
// engine home's data dir after engines have exited. Failures are ignored: the
// caller still proceeds to remove on-disk state.
func stopManagedPostgresBestEffort(s config.Settings) {
	if s.Socket == "" {
		return
	}
	dir := ManagedPGDir(s)
	dataDir := managedDataDir(dir)
	if !postmasterProcessAlive(dataDir) {
		removeStalePostmasterPid(dataDir)
		return
	}
	_ = (&adoptedSupervisor{dir: dir, dataDir: dataDir}).Stop()
}
