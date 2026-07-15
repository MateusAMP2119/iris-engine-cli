package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the store half of `iris pipeline promote`: the meta write that
// flips one pipeline's per-pipeline data_mode from disposable to permanent. The
// flip is the promote op's entire meta footprint -- one guarded single-row UPDATE
// riding the single Writer -- and it touches only the named pipeline's row; the
// gate that decides WHETHER the flip may run lives in the dispatch promote op.

// TestPromotePipelineFlipsDataMode proves Writer.PromotePipeline issues exactly
// one meta statement: the pipelines data_mode flip to permanent, scoped to the
// one named pipeline by bound parameter -- disposable to permanent, nothing
// else touched.
func TestPromotePipelineFlipsDataMode(t *testing.T) {
	rec := storetest.NewWriteRecorder()
	w := store.NewWriter(rec)

	if err := w.PromotePipeline(context.Background(), "etl"); err != nil {
		t.Fatalf("PromotePipeline: %v", err)
	}

	stmts := rec.Statements()
	if len(stmts) != 1 {
		t.Fatalf("PromotePipeline issued %d statements, want exactly 1", len(stmts))
	}
	sql := stmts[0].SQL
	if !strings.Contains(sql, "UPDATE pipelines") {
		t.Errorf("flip statement does not update the pipelines registry:\n%s", sql)
	}
	if !strings.Contains(sql, "data_mode = 'permanent'") {
		t.Errorf("flip statement does not set data_mode to permanent:\n%s", sql)
	}
	if !strings.Contains(sql, "WHERE name = $1") {
		t.Errorf("flip statement is not scoped to one pipeline by bound name:\n%s", sql)
	}
	if len(stmts[0].Args) != 1 || stmts[0].Args[0] != "etl" {
		t.Errorf("flip statement args = %v, want exactly the pipeline name %q", stmts[0].Args, "etl")
	}
}
