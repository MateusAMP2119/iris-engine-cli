package dispatch_test

import (
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
)

// TestEventsSequenceAndWake proves the watermark's contract: the sequence is
// monotonic under bumps, a bump with no waiter leaves exactly one pending wake
// token (coalescing, never blocking), and a receive consumes the token so a
// second receive would block until the next bump.
func TestEventsSequenceAndWake(t *testing.T) {
	t.Run("sequence-monotonic-wake-coalescing", func(t *testing.T) {
		e := dispatch.NewEvents()
		if got := e.Seq(); got != 0 {
			t.Fatalf("fresh watermark Seq() = %d, want 0", got)
		}

		// Two bumps with no waiter: the sequence counts both, the wake channel
		// coalesces to one pending token.
		e.Bump()
		e.Bump()
		if got := e.Seq(); got != 2 {
			t.Fatalf("Seq() after two bumps = %d, want 2", got)
		}
		select {
		case <-e.Wake():
		default:
			t.Fatalf("no pending wake token after bumps, want exactly one (coalescing, not dropping)")
		}
		select {
		case <-e.Wake():
			t.Fatalf("second pending wake token, want the bumps coalesced to one")
		default:
		}

		// A bump after the token was consumed leaves a fresh token.
		e.Bump()
		if got := e.Seq(); got != 3 {
			t.Fatalf("Seq() after third bump = %d, want 3", got)
		}
		select {
		case <-e.Wake():
		default:
			t.Fatalf("no pending wake token after a fresh bump")
		}
	})
}
