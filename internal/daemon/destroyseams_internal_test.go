package daemon

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the destroyer's production teardown seams: the reverter runs
// the journal-driven revert scoped to the destroyed pipeline with live run
// attribution, and the object deleter frees exactly the named content-addressed
// artifact files, idempotently.

// revertDataSpy is a dataPlane recording the wipe target ExecuteWipe received.
type revertDataSpy struct {
	controlDataFake
	targets []pg.WipeTarget
}

func (f *revertDataSpy) ExecuteWipe(_ context.Context, target pg.WipeTarget) (pg.WipeResult, error) {
	f.targets = append(f.targets, target)
	return pg.WipeResult{}, nil
}

func TestDestroyReverterScopedWipe(t *testing.T) {
	t.Run("destroy-reverter-scoped-wipe", func(t *testing.T) {
		meta := storetest.New()
		target := seedRun(t, meta, "victim", store.RunSucceeded)
		other := seedRun(t, meta, "bystander", store.RunSucceeded)
		data := &revertDataSpy{}

		rev := destroyReverter{reader: meta, data: data}
		if err := rev.RevertUnpromoted(context.Background(), "victim"); err != nil {
			t.Fatalf("RevertUnpromoted: %v", err)
		}
		if len(data.targets) != 1 {
			t.Fatalf("ExecuteWipe called %d times, want 1", len(data.targets))
		}
		got := data.targets[0]
		if got.Pipeline != "victim" {
			t.Errorf("wipe target pipeline = %q, want the destroyed pipeline", got.Pipeline)
		}
		// The attribution map carries every run, so the wipe's covers() resolves
		// journal entries to pipelines and reverts only the target's.
		want := map[int64]string{parseRunID(target.ID): "victim", parseRunID(other.ID): "bystander"}
		if !reflect.DeepEqual(got.RunPipeline, want) {
			t.Errorf("wipe attribution = %v, want %v", got.RunPipeline, want)
		}
	})
}

func TestDestroyObjectDeleterFreesBytes(t *testing.T) {
	t.Run("destroy-object-deleter-frees-bytes", func(t *testing.T) {
		root := t.TempDir()
		objects := store.NewObjectStore(root)
		hashA, _, err := objects.Put(bytes.NewReader([]byte("binary-a")))
		if err != nil {
			t.Fatalf("Put a: %v", err)
		}
		hashKeep, _, err := objects.Put(bytes.NewReader([]byte("binary-keep")))
		if err != nil {
			t.Fatalf("Put keep: %v", err)
		}

		del := destroyObjectDeleter{objects: objects}
		if err := del.DeleteObjects(context.Background(), "victim", []string{hashA, "feed00000000"}); err != nil {
			t.Fatalf("DeleteObjects: %v", err)
		}
		if _, err := os.Stat(objects.Path(hashA)); !os.IsNotExist(err) {
			t.Errorf("the destroyed pipeline's artifact bytes survived (stat err %v)", err)
		}
		if _, err := os.Stat(objects.Path(hashKeep)); err != nil {
			t.Errorf("an unrelated object was disturbed: %v", err)
		}
	})
}
