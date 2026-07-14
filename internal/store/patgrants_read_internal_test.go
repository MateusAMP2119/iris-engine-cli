package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// This file proves the data-PAT grant-ledger read over a scripted pool (no live
// Postgres): grants group under their role in order, and a grant-less role (the
// LEFT JOIN's empty row) still appears so it is reconciled and its strays
// reported.

func TestPgxDataPATGrantsReader(t *testing.T) {
	t.Run("pgx-data-pat-grants-reader", func(t *testing.T) {
		pool := &retentionScriptPool{bySQL: map[string][][]any{
			selectDataPATGrantsSQL: {
				{"iris_pat_a", "analytics", "orders", "amount", "read"},
				{"iris_pat_a", "analytics", "orders", "customer", "read"},
				{"iris_pat_b", "", "", "", ""}, // minted role, no grants rows
			},
		}}
		got, err := newPgxDataPATGrantsReader(pool).DataPATRoleGrants(context.Background())
		if err != nil {
			t.Fatalf("DataPATRoleGrants: %v", err)
		}
		want := []RoleGrantLedger{
			{Role: "iris_pat_a", Grants: []declare.FieldGrant{
				{Schema: "analytics", Table: "orders", Field: "amount", Access: declare.AccessRead},
				{Schema: "analytics", Table: "orders", Field: "customer", Access: declare.AccessRead},
			}},
			{Role: "iris_pat_b"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("DataPATRoleGrants =\n %+v, want\n %+v", got, want)
		}
	})
}
