package daemon

import "testing"

// TestSealDue proves the pure seal condition: a partition seals only when it is
// past the row threshold, every in-flight run writing into it has finished (no run
// running), and it holds rows. A below-threshold partition, an empty partition, an
// in-flight run, or a disabled (non-positive) threshold all defer the seal.
func TestSealDue(t *testing.T) {
	t.Run("seal-condition", func(t *testing.T) {
		cases := []struct {
			name         string
			residentRows int64
			threshold    int64
			runningRuns  int64
			want         bool
		}{
			{"past threshold, no in-flight run seals", 10, 5, 0, true},
			{"exactly at threshold seals", 5, 5, 0, true},
			{"below threshold does not seal", 4, 5, 0, false},
			{"in-flight run defers the seal", 10, 5, 1, false},
			{"in-flight run defers even far past threshold", 1000, 5, 2, false},
			{"empty resident partition never seals", 0, 5, 0, false},
			{"disabled threshold never seals", 10, 0, 0, false},
			{"negative threshold never seals", 10, -1, 0, false},
			{"below threshold with in-flight run does not seal", 3, 5, 1, false},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				if got := sealDue(c.residentRows, c.threshold, c.runningRuns); got != c.want {
					t.Errorf("sealDue(rows=%d, threshold=%d, running=%d) = %v, want %v",
						c.residentRows, c.threshold, c.runningRuns, got, c.want)
				}
			})
		}
	})
}
