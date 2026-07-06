package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedInstance is a scriptable pgInstance for exercising embeddedSupervisor's
// failure paths without a real Postgres download: Start returns startErr, and each
// Stop call returns the next error from stopErrs (nil once exhausted), recording how
// many times each was invoked.
type scriptedInstance struct {
	startErr error
	stopErrs []error
	starts   int
	stops    int
}

func (s *scriptedInstance) Start() error {
	s.starts++
	return s.startErr
}

func (s *scriptedInstance) Stop() error {
	i := s.stops
	s.stops++
	if i < len(s.stopErrs) {
		return s.stopErrs[i]
	}
	return nil
}

// newScriptedSupervisor builds an embeddedSupervisor wired to a fixed scripted
// instance, with the managed directory pointed at a fresh temp dir so
// alreadyMaterialized() is false and EnsureInstalled proceeds to Start/Stop.
func newScriptedSupervisor(t *testing.T, inst *scriptedInstance) *embeddedSupervisor {
	t.Helper()
	dir := t.TempDir()
	return &embeddedSupervisor{
		cfg:         SupervisorConfig{Dir: dir, DataDir: dir + "/data"},
		newInstance: func() pgInstance { return inst },
	}
}

// TestEmbeddedEnsureInstalledStopFailureNeverStrands proves that when the managed
// Postgres starts during install but the subsequent stop fails, the supervisor never
// silently strands a running postgres subprocess: it retries the stop best-effort,
// and if that also fails it retains the instance handle (so a later Stop can still
// reach the process) and returns an error that names the orphan risk and its
// remediation (specification section 2: managed Postgres "stopped on shutdown").
//
// spec: S02/managed-pg-subprocess-lifecycle
func TestEmbeddedEnsureInstalledStopFailureNeverStrands(t *testing.T) {
	boom := errors.New("pg_ctl stop failed")

	t.Run("persistent stop failure surfaces the orphan risk and retains the handle", func(t *testing.T) {
		inst := &scriptedInstance{stopErrs: []error{boom, boom}}
		s := newScriptedSupervisor(t, inst)

		err := s.EnsureInstalled(context.Background())
		if err == nil {
			t.Fatal("EnsureInstalled = nil, want an error naming the orphan risk after a failed stop")
		}
		if !errors.Is(err, boom) {
			t.Errorf("error does not wrap the underlying stop failure: %v", err)
		}
		// The message must warn that a postgres subprocess may still be running, with a
		// remediation -- never a silent strand.
		low := strings.ToLower(err.Error())
		if !strings.Contains(low, "still") && !strings.Contains(low, "running") {
			t.Errorf("error does not name the orphan risk (still running): %v", err)
		}
		if !strings.Contains(low, "pg_ctl") {
			t.Errorf("error does not name a remediation (pg_ctl stop): %v", err)
		}
		// Best-effort cleanup happened: the stop was retried at least once.
		if inst.stops < 2 {
			t.Errorf("stop attempted %d times, want a best-effort retry (>= 2)", inst.stops)
		}
		// The handle is retained so a later Stop() can still reach the process.
		if s.running != inst {
			t.Error("supervisor did not retain the running instance handle; a later Stop cannot reach the process")
		}
	})

	t.Run("stop that succeeds on retry leaves nothing running", func(t *testing.T) {
		inst := &scriptedInstance{stopErrs: []error{boom}} // first stop fails, retry succeeds
		s := newScriptedSupervisor(t, inst)

		if err := s.EnsureInstalled(context.Background()); err != nil {
			t.Fatalf("EnsureInstalled = %v, want nil once the retry stops the instance", err)
		}
		if inst.stops < 2 {
			t.Errorf("stop attempted %d times, want a retry after the first failure", inst.stops)
		}
		if s.running != nil {
			t.Error("supervisor retained a handle after a successful stop; nothing should be running")
		}
	})

	t.Run("clean install stops the instance and retains nothing", func(t *testing.T) {
		inst := &scriptedInstance{}
		s := newScriptedSupervisor(t, inst)

		if err := s.EnsureInstalled(context.Background()); err != nil {
			t.Fatalf("EnsureInstalled = %v, want nil", err)
		}
		if inst.starts != 1 || inst.stops != 1 {
			t.Errorf("clean install started %d / stopped %d times, want 1 / 1", inst.starts, inst.stops)
		}
		if s.running != nil {
			t.Error("clean install left a running handle; the install leg must leave no server running")
		}
	})
}
