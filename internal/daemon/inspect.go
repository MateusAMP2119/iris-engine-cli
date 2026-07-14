package daemon

import (
	"context"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's engine-inspect plane: the api.InspectHandler behind GET
// /inspect (and therefore behind `iris engine inspect` -- one route, one payload).
// The engine-table DDL is an embedded model (create-if-missing at bootstrap), so
// the dump renders that model -- the meta control tables followed by the data
// journal and its initial partition -- and touches no database at all: no
// connection, no row read, no write. That is the mutation-free guarantee by
// construction, not by discipline.

// inspectPlane is the api.InspectHandler over the embedded schema models. It holds
// nothing: the dump is a pure render.
type inspectPlane struct{}

// compile-time proof the plane satisfies the mux's inspect seam.
var _ api.InspectHandler = inspectPlane{}

// NewInspectPlane builds the inspect handler the daemon wires into the api mux.
// It takes no connection or store seam on purpose: the DDL dump renders the
// embedded schema models only, so inspect can never mutate engine state.
func NewInspectPlane() api.InspectHandler { return inspectPlane{} }

// Inspect renders the engine-table DDL: the meta control tables (store's embedded
// schema, in its bootstrap emission order) followed by the data journal and its
// initial open partition (pg's embedded model). Every statement is the same
// create-if-missing text bootstrap applies.
func (inspectPlane) Inspect(context.Context) (api.InspectPayload, error) {
	ddl := store.MetaSchema().DDL()
	ddl = append(ddl, pg.JournalTable().DDL()...)
	ddl = append(ddl, pg.InitialPartition().CreateDDL())
	return api.InspectPayload{DDL: ddl}, nil
}
