package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestResolveManagedPasswordRaceConverges proves the managed superuser credential is
// persisted atomically: several candidates resolving the password concurrently on
// the same fresh managed-Postgres directory all converge on the one credential the
// winner persisted, rather than each minting and returning a conflicting one (the
// TOCTOU a read-then-write would leave). Both databases in a managed cluster are
// keyed by this single credential, so a split here would leave losers unable to
// reconnect.
//
// spec: S02/one-leader-sole-dispatcher
func TestResolveManagedPasswordRaceConverges(t *testing.T) {
	t.Run("S02/one-leader-sole-dispatcher", func(t *testing.T) {
		dir := t.TempDir()
		const candidates = 12

		var (
			wg      sync.WaitGroup
			start   = make(chan struct{})
			mu      sync.Mutex
			results []string
		)
		wg.Add(candidates)
		for i := 0; i < candidates; i++ {
			go func() {
				defer wg.Done()
				<-start // barrier: maximize the race on the empty directory.
				pw, err := resolveManagedPassword(dir)
				if err != nil {
					t.Errorf("resolveManagedPassword: %v", err)
					return
				}
				mu.Lock()
				results = append(results, pw)
				mu.Unlock()
			}()
		}
		close(start)
		wg.Wait()

		if len(results) != candidates {
			t.Fatalf("got %d results, want %d (some resolves failed)", len(results), candidates)
		}
		first := results[0]
		if first == "" {
			t.Fatal("resolveManagedPassword returned an empty credential")
		}
		for i, pw := range results {
			if pw != first {
				t.Fatalf("candidate %d resolved a different credential (%q vs %q); concurrent resolves must converge on one", i, pw, first)
			}
		}

		// The persisted file matches the converged credential exactly.
		raw, err := os.ReadFile(filepath.Join(dir, superuserPasswordFile)) //nolint:gosec // G304: path is the test's own temp workspace, never user or network input.
		if err != nil {
			t.Fatalf("read persisted credential: %v", err)
		}
		if got := strings.TrimSpace(string(raw)); got != first {
			t.Errorf("persisted credential = %q, want the converged %q", got, first)
		}
	})

	t.Run("a second resolve reuses the persisted credential", func(t *testing.T) {
		dir := t.TempDir()
		first, err := resolveManagedPassword(dir)
		if err != nil {
			t.Fatalf("first resolve: %v", err)
		}
		second, err := resolveManagedPassword(dir)
		if err != nil {
			t.Fatalf("second resolve: %v", err)
		}
		if first != second {
			t.Errorf("second resolve minted a new credential %q, want the persisted %q", second, first)
		}
	})
}
