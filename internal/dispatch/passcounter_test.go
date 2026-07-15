package dispatch_test

import (
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
)

// TestPassCounter proves the per-lane loop pass counter's mechanics: the hook counts
// completed lane passes per lane (a clock-free count, incremented once per
// PassReport), Counts returns a defensive copy, and Reset zeroes every lane -- the
// primitive the daemon holds per leadership term ("loop passes completed since daemon
// start (a leader-held runtime counter, reset on restart and leader change)"). The
// daemon-side term wiring is proven in internal/daemon.
func TestPassCounter(t *testing.T) {
	pc := dispatch.NewPassCounter()
	hook := pc.Hook()

	// A fresh counter (a freshly started daemon) holds no counts.
	if got := pc.Counts(); len(got) != 0 {
		t.Fatalf("fresh counter counts = %v, want empty (resets on daemon restart by construction)", got)
	}

	// One increment per completed lane pass, keyed by lane.
	hook(dispatch.PassReport{Lane: "ingest"})
	hook(dispatch.PassReport{Lane: "ingest"})
	hook(dispatch.PassReport{Lane: "side"})
	counts := pc.Counts()
	if counts["ingest"] != 2 || counts["side"] != 1 {
		t.Fatalf("counts = %v, want ingest 2, side 1", counts)
	}

	// Counts is a copy: mutating the snapshot never reaches the counter.
	counts["ingest"] = 99
	if got := pc.Counts()["ingest"]; got != 2 {
		t.Fatalf("counter state changed through a returned snapshot: ingest = %d, want 2", got)
	}

	// Reset zeroes every lane: the leader-change (new term) semantics.
	pc.Reset()
	if got := pc.Counts(); len(got) != 0 {
		t.Fatalf("post-reset counts = %v, want empty", got)
	}
	hook(dispatch.PassReport{Lane: "ingest"})
	if got := pc.Counts()["ingest"]; got != 1 {
		t.Fatalf("post-reset ingest = %d, want 1 (counting restarts from zero)", got)
	}
}
