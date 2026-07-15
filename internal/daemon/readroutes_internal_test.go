package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the three E14 read-route planes compose the store's plain-MVCC
// reads (and dispatch's pure gate) into the wire payloads the mux renders and the
// CLI decodes, over fakes with no live Postgres (integration tier). The api-level
// route mechanics are proven separately over the mux; these prove the daemon-side
// composition the production wiring installs: the runs collection carries its
// consumed upstream ids and replayed_from only under include=inputs, the trace walk
// climbs ancestry up and descendants down, and the gate route resolves the same
// ledger the pipeline-show readout does.

// fakeRunLineageReader is an in-memory store.RunLineageReader.
type fakeRunLineageReader struct {
	all  []store.RunLineage
	byID map[int64]store.RunLineage
	err  error
}

func (f fakeRunLineageReader) RunLineages(context.Context) ([]store.RunLineage, error) {
	return f.all, f.err
}

func (f fakeRunLineageReader) RunLineage(_ context.Context, id int64) (store.RunLineage, bool, error) {
	if f.err != nil {
		return store.RunLineage{}, false, f.err
	}
	rl, ok := f.byID[id]
	return rl, ok, nil
}

func TestRunsPlaneListRunsIncludeInputs(t *testing.T) {
	rf := int64(10)
	reader := fakeRunLineageReader{all: []store.RunLineage{
		{ID: 42, Pipeline: "load", State: store.RunDeadLettered, ReplayedFrom: &rf, Inputs: []int64{39, 40}},
		{ID: 41, Pipeline: "extract", State: store.RunSucceeded},
	}}
	p := newRunsPlane(reader, nil)

	// include=inputs: the lineage attributes ride the rows.
	res, err := p.ListRuns(context.Background(), true)
	if err != nil {
		t.Fatalf("ListRuns(include): %v", err)
	}
	col, ok := res.(api.RunsCollection)
	if !ok {
		t.Fatalf("ListRuns returned %T, want api.RunsCollection", res)
	}
	if len(col.Runs) != 2 {
		t.Fatalf("ListRuns rows = %d, want 2", len(col.Runs))
	}
	r0 := col.Runs[0]
	if r0.ID != "42" || r0.Pipeline != "load" || r0.State != "dead_lettered" {
		t.Errorf("row0 = %+v, want run 42 load dead_lettered", r0)
	}
	if len(r0.Inputs) != 2 || r0.Inputs[0] != "39" || r0.Inputs[1] != "40" {
		t.Errorf("row0 inputs = %v, want [39 40] as decimal strings", r0.Inputs)
	}
	if r0.ReplayedFrom != "10" {
		t.Errorf("row0 replayed_from = %q, want 10", r0.ReplayedFrom)
	}

	// include unset: bare rows, no lineage attributes.
	bare, err := p.ListRuns(context.Background(), false)
	if err != nil {
		t.Fatalf("ListRuns(bare): %v", err)
	}
	b0 := bare.(api.RunsCollection).Runs[0]
	if b0.Inputs != nil || b0.ReplayedFrom != "" {
		t.Errorf("bare row carried lineage attrs %+v; include=inputs gates them", b0)
	}
}

func TestRunsPlaneListRunsError(t *testing.T) {
	p := newRunsPlane(fakeRunLineageReader{err: errors.New("boom")}, nil)
	if _, err := p.ListRuns(context.Background(), true); err == nil {
		t.Fatal("ListRuns swallowed the reader error; want it surfaced")
	}
}

func TestRunsPlaneGetRun(t *testing.T) {
	reader := fakeRunLineageReader{byID: map[int64]store.RunLineage{
		7: {ID: 7, Pipeline: "transform", State: store.RunRunning, Inputs: []int64{5}},
	}}
	p := newRunsPlane(reader, nil)

	res, err := p.GetRun(context.Background(), "7", true)
	if err != nil {
		t.Fatalf("GetRun(7): %v", err)
	}
	row, ok := res.(api.RunRow)
	if !ok {
		t.Fatalf("GetRun returned %T, want api.RunRow", res)
	}
	if row.ID != "7" || len(row.Inputs) != 1 || row.Inputs[0] != "5" {
		t.Errorf("GetRun(7) = %+v, want run 7 with input 5", row)
	}

	if _, err := p.GetRun(context.Background(), "999", true); err == nil {
		t.Error("GetRun(absent) returned no error; want not-found")
	}
	if _, err := p.GetRun(context.Background(), "nope", true); err == nil {
		t.Error("GetRun(malformed id) returned no error; want a parse error")
	}
}

// seedTraceLineage builds a lineage where run 42 consumed 39 and 40, and 39 consumed
// 7 -- a two-level ancestry the up-walk climbs and the down-walk inverts.
func seedTraceLineage() store.ProvenanceLineage {
	var lin store.ProvenanceLineage
	for _, id := range []int64{7, 39, 40, 42} {
		lin.Runs = append(lin.Runs, struct {
			RunID               int64
			Pipeline            string
			State               string
			ArtifactHash        *string
			DeclarationChecksum string
			SnapshotLSN         *string
			JournalFloor        *int64
			JournalCeiling      *int64
		}{RunID: id, Pipeline: "p", State: "succeeded"})
	}
	lin.Inputs = []struct {
		RunID         int64
		UpstreamRunID int64
	}{
		{RunID: 42, UpstreamRunID: 39},
		{RunID: 42, UpstreamRunID: 40},
		{RunID: 39, UpstreamRunID: 7},
	}
	return lin
}

func TestRunTracePlaneUpAndDown(t *testing.T) {
	fake := storetest.New()
	fake.SetProvenanceLineage(seedTraceLineage())
	p := newRunTracePlane(fake, nil)

	// Up (default): full ancestry of 42 -> {39,40} then 39 -> 7.
	up, err := p.Trace(context.Background(), "42", "")
	if err != nil {
		t.Fatalf("Trace up: %v", err)
	}
	payload, ok := up.(api.RunTracePayload)
	if !ok {
		t.Fatalf("Trace returned %T, want api.RunTracePayload", up)
	}
	if payload.Run != "42" || payload.Direction != "up" {
		t.Errorf("trace header = %+v, want run 42 direction up", payload)
	}
	if len(payload.Ancestry) != 3 {
		t.Fatalf("up ancestry = %+v, want 3 edges (42->39, 42->40, 39->7)", payload.Ancestry)
	}
	// Depth-1 edges first (42's parents), then the depth-2 edge 39->7.
	if payload.Ancestry[2].RunID != "39" || payload.Ancestry[2].UpstreamRunID != "7" || payload.Ancestry[2].Depth != 2 {
		t.Errorf("deepest edge = %+v, want 39->7 depth 2", payload.Ancestry[2])
	}

	// Down: who consumed 7 -> 39, then who consumed 39 -> 42.
	down, err := p.Trace(context.Background(), "7", "down")
	if err != nil {
		t.Fatalf("Trace down: %v", err)
	}
	dp := down.(api.RunTracePayload)
	if dp.Direction != "down" || len(dp.Ancestry) != 2 {
		t.Fatalf("down walk = %+v, want 2 edges (39 consumed 7, 42 consumed 39)", dp.Ancestry)
	}
}

func TestRunTracePlaneNotFoundAndBadID(t *testing.T) {
	fake := storetest.New()
	fake.SetProvenanceLineage(seedTraceLineage())
	p := newRunTracePlane(fake, nil)

	if _, err := p.Trace(context.Background(), "999", "up"); err == nil {
		t.Error("Trace(absent run) returned no error; want not-found")
	}
	if _, err := p.Trace(context.Background(), "nope", "up"); err == nil {
		t.Error("Trace(malformed id) returned no error; want a parse error")
	}
}

func TestPipelineGatePlaneLedger(t *testing.T) {
	// load depends_on extract; extract's latest run is an unconsumed success -> the
	// gate is open for that edge.
	fake := storetest.NewShow().
		SetDetail("load", store.PipelineDetail{Folder: "pipelines/load"}).
		SetDetail("extract", store.PipelineDetail{Folder: "pipelines/extract"}).
		AddEdge("load", "extract").
		SetLatestRun("extract", store.LatestRunInfo{ID: 42, State: store.RunSucceeded})
	p := newPipelineGatePlane(fake, nil)

	res, err := p.Gate(context.Background(), "load")
	if err != nil {
		t.Fatalf("Gate(load): %v", err)
	}
	payload, ok := res.(api.PipelineGatePayload)
	if !ok {
		t.Fatalf("Gate returned %T, want api.PipelineGatePayload", res)
	}
	if payload.Pipeline != "load" || len(payload.Gate) != 1 {
		t.Fatalf("gate payload = %+v, want one edge for load", payload)
	}
	row := payload.Gate[0]
	if row.Upstream != "extract" || row.Verdict != "open" || row.LatestRunID != "42" {
		t.Errorf("gate row = %+v, want extract open latest 42", row)
	}
}

func TestPipelineGatePlaneUnregistered(t *testing.T) {
	p := newPipelineGatePlane(storetest.NewShow(), nil)
	if _, err := p.Gate(context.Background(), "ghost"); err == nil {
		t.Error("Gate(unregistered) returned no error; want not-found")
	}
}
