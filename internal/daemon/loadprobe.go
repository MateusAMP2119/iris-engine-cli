package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// This file is the daemon's host-load probe: the one place the engine samples
// what it costs the machine. It shells out to the host's ps(1) -- present on
// every darwin and linux host the daemon runs on -- with a single `ps -axo
// pid=,pgid=,ppid=,rss=,pcpu=` snapshot, cgo-free and dependency-free. The ps
// plane sums the rows two ways: the engine's load is the daemon's descendant
// tree plus the managed postmaster's (Postgres daemonizes into its own session,
// so parentage -- not the process group -- finds its backends), and each
// running run's load is its recorded process-group id (runs.handle), the same
// group a cancel kills. The probe is best-effort by contract: a host without ps
// answers an error and the ps payload carries null load, never a fabricated
// zero.

// loadProbeTimeout bounds one ps(1) snapshot so a wedged host tool can never
// stall the /ps route.
const loadProbeTimeout = 3 * time.Second

// procSample is one process's sampled host load: its process group and parent
// (the two summing keys), CPU percentage, and resident memory.
type procSample struct {
	// PID is the process id.
	PID int
	// PGID is the process-group id a run's sample is summed under.
	PGID int
	// PPID is the parent process id the engine's descendant walk follows.
	PPID int
	// CPUPercent is ps's %cpu reading for the process.
	CPUPercent float64
	// RSSBytes is the resident set size in bytes (ps reports KiB).
	RSSBytes int64
}

// loadProber samples the host's process table. The production implementation
// shells out to ps(1); tests inject a fixed sample.
type loadProber interface {
	// Sample returns one snapshot of every host process's load.
	Sample(ctx context.Context) ([]procSample, error)
}

// psProbe is the production loadProber over the host's ps(1).
type psProbe struct{}

// compile-time proof the ps(1) probe satisfies the prober seam.
var _ loadProber = psProbe{}

// Sample runs one `ps -axo pid=,pgid=,ppid=,rss=,pcpu=` snapshot and parses it.
// The flag set is the POSIX-portable intersection darwin and linux procps share.
func (psProbe) Sample(ctx context.Context) ([]procSample, error) {
	pctx, cancel := context.WithTimeout(ctx, loadProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(pctx, "ps", "-axo", "pid=,pgid=,ppid=,rss=,pcpu=").Output()
	if err != nil {
		return nil, fmt.Errorf("daemon: load probe: ps: %w", err)
	}
	return parsePSSample(out)
}

// parsePSSample parses ps(1)'s five-column snapshot (pid, pgid, ppid, rss in
// KiB, %cpu). A malformed line is an error, not a skip: the probe is
// best-effort at the call site, so a broken host tool surfaces rather than
// thinning the sample.
func parsePSSample(out []byte) ([]procSample, error) {
	var samples []procSample
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 5 {
			return nil, fmt.Errorf("daemon: load probe: malformed ps line %q", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("daemon: load probe: pid %q: %w", fields[0], err)
		}
		pgid, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("daemon: load probe: pgid %q: %w", fields[1], err)
		}
		ppid, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("daemon: load probe: ppid %q: %w", fields[2], err)
		}
		rssKiB, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("daemon: load probe: rss %q: %w", fields[3], err)
		}
		cpu, err := strconv.ParseFloat(strings.ReplaceAll(fields[4], ",", "."), 64)
		if err != nil {
			return nil, fmt.Errorf("daemon: load probe: %%cpu %q: %w", fields[4], err)
		}
		samples = append(samples, procSample{PID: pid, PGID: pgid, PPID: ppid, CPUPercent: cpu, RSSBytes: rssKiB * 1024})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("daemon: load probe: scan ps output: %w", err)
	}
	return samples, nil
}
