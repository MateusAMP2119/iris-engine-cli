package tui

import (
	"fmt"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
)

// cpuText renders a sampled CPU load, or "-" when the host was not probed.
func cpuText(l *api.PsLoad) string {
	if l == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", l.CPUPercent)
}

// memText renders a sampled resident memory load, or "-" when the host was not
// probed.
func memText(l *api.PsLoad) string {
	if l == nil {
		return "-"
	}
	return memBytes(l.RSSBytes)
}

// memBytes renders a byte count human-readably (KiB/MiB/GiB, one decimal).
func memBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// exitCodeCell renders a run row's exit code, or "-" while the run carries none.
func exitCodeCell(code *int) string {
	if code == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *code)
}

// shortDigest abbreviates a sha256 for the human line.
func shortDigest(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}
