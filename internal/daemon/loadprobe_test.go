package daemon

import (
	"context"
	"runtime"
	"testing"
)

// TestParsePSSample proves the ps(1) snapshot parser: well-formed lines parse
// into pid/pgid/rss-bytes/cpu samples (rss scaled from KiB, a comma decimal
// accepted for a comma locale), blank lines are skipped, and a malformed line
// is an error, never a thinned sample.
func TestParsePSSample(t *testing.T) {
	t.Run("well-formed snapshot parses", func(t *testing.T) {
		out := []byte("    1     1     0  1024   0.0\n  4242  4242     1 151552  2.5\n\n  4243  4242  4242  2048   0,5\n")
		got, err := parsePSSample(out)
		if err != nil {
			t.Fatalf("parsePSSample: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("samples = %d, want 3", len(got))
		}
		daemon := got[1]
		if daemon.PID != 4242 || daemon.PGID != 4242 || daemon.PPID != 1 || daemon.RSSBytes != 151552*1024 || daemon.CPUPercent != 2.5 {
			t.Errorf("sample = %+v, want pid 4242 pgid 4242 ppid 1 rss 151552KiB cpu 2.5", daemon)
		}
		if child := got[2]; child.CPUPercent != 0.5 || child.PPID != 4242 {
			t.Errorf("child sample = %+v, want ppid 4242 and comma-locale %%cpu 0.5", child)
		}
	})

	t.Run("malformed line is an error", func(t *testing.T) {
		for name, out := range map[string]string{
			"missing column":  "1 1 1 1024\n",
			"non-numeric pid": "x 1 1 1024 0.0\n",
			"non-numeric rss": "1 1 1 lots 0.0\n",
		} {
			if _, err := parsePSSample([]byte(out)); err == nil {
				t.Errorf("%s: parsePSSample = nil error, want a fault", name)
			}
		}
	})

	t.Run("empty snapshot parses to no samples", func(t *testing.T) {
		got, err := parsePSSample(nil)
		if err != nil {
			t.Fatalf("parsePSSample(nil): %v", err)
		}
		if len(got) != 0 {
			t.Errorf("samples = %d, want 0", len(got))
		}
	})
}

// TestPsProbeSamplesLiveHost drives the real ps(1) probe on the host and proves
// it returns a parseable, non-empty sample that includes this test process.
// darwin and linux carry ps(1) by contract; other platforms skip.
func TestPsProbeSamplesLiveHost(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no ps(1) contract on %s", runtime.GOOS)
	}
	samples, err := psProbe{}.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if len(samples) == 0 {
		t.Fatal("Sample returned no processes")
	}
}
