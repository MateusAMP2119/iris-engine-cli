package store_test

import (
	"context"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the access-ledger record path apply and data-PAT mint drive
// (specification sections 3, 4, 7): the declared reads/writes and a data PAT's
// per-field grants are recorded in meta as one atomic full-role rewrite over the
// single writer, with no live Postgres.

// grantRow is a recorded grants insert reduced to its bound (schema, table, field,
// access) tuple, for order-independent set assertions.
type grantRow struct {
	schema, table, field, access string
}

// recordedGrantRows extracts the (schema, table, field, access) tuple of each
// grants insert a WriteRecorder captured for pgRole, asserting each row also binds
// that pg_role in position one.
func recordedGrantRows(t *testing.T, rec *storetest.WriteRecorder, pgRole string) []grantRow {
	t.Helper()
	var rows []grantRow
	for _, s := range stmtsContaining(rec.Statements(), "INSERT INTO grants") {
		if len(s.Args) != 5 {
			t.Fatalf("grants insert has %d args, want 5: %v", len(s.Args), s.Args)
		}
		if s.Args[0] != pgRole {
			t.Errorf("grants insert pg_role = %v, want %q", s.Args[0], pgRole)
		}
		rows = append(rows, grantRow{
			schema: s.Args[1].(string), table: s.Args[2].(string),
			field: s.Args[3].(string), access: s.Args[4].(string),
		})
	}
	return rows
}

// TestApplyRecordsAccessMeta proves apply records a pipeline's declared reads and
// writes in the meta access ledger, driving ReplaceGrants from the declaration's
// reads/writes: exactly one per-field grant row per declared (table, field),
// tagged read for a reads entry and write for a writes entry, written as one
// atomic full-role rewrite. Nothing beyond the declared entries is recorded.
//
// spec: S03/apply-records-access-meta
func TestApplyRecordsAccessMeta(t *testing.T) {
	t.Run("S03/apply-records-access-meta", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)

		reads := []declare.Access{{Table: "analytics.orders", Fields: []string{"id", "amount"}}}
		writes := []declare.Access{{Table: "raw.orders_staging", Fields: []string{"id"}}}

		if err := w.RecordAccessGrants(context.Background(), "iris_load_orders", reads, writes); err != nil {
			t.Fatalf("RecordAccessGrants: %v", err)
		}

		// One atomic transaction carrying the whole rewrite (clear then re-insert).
		txns := rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("RecordAccessGrants committed %d transactions, want 1 (atomic full-role rewrite)", len(txns))
		}

		got := recordedGrantRows(t, rec, "iris_load_orders")
		want := []grantRow{
			{"analytics", "orders", "id", "read"},
			{"analytics", "orders", "amount", "read"},
			{"raw", "orders_staging", "id", "write"},
		}
		if !sameGrantSet(got, want) {
			t.Errorf("recorded grants = %v, want exactly the declared reads/writes %v (nothing more)", got, want)
		}
	})
}

// TestDataPATGrantsRecordedPerField proves a data PAT's grants are recorded per
// field in the access ledger at mint (specification section 7), whether minted
// field-explicit, via a bare schema.table (which expands to every field declared
// at mint time), or via --endpoint (which expands to the endpoint's source
// fields). Every recorded grant is a read grant (a data PAT is read-only), one row
// per field.
//
// spec: S07/data-pat-grants-recorded-per-field
func TestDataPATGrantsRecordedPerField(t *testing.T) {
	declared := map[string][]string{"analytics.orders": {"id", "customer_id", "amount"}}
	endpoints := map[string]declare.EndpointSource{
		"orders_by_customer": {Source: "analytics.orders", Fields: []string{"id", "customer_id"}},
	}

	cases := []struct {
		name  string
		reads []declare.DataPATRead
		want  []grantRow
	}{
		{
			name:  "field-explicit records the one field",
			reads: []declare.DataPATRead{{Table: "analytics.orders", Field: "amount"}},
			want:  []grantRow{{"analytics", "orders", "amount", "read"}},
		},
		{
			name:  "bare schema.table records every declared field",
			reads: []declare.DataPATRead{{Table: "analytics.orders"}},
			want: []grantRow{
				{"analytics", "orders", "id", "read"},
				{"analytics", "orders", "customer_id", "read"},
				{"analytics", "orders", "amount", "read"},
			},
		},
		{
			name:  "endpoint records the endpoint source fields",
			reads: []declare.DataPATRead{{Endpoint: "orders_by_customer"}},
			want: []grantRow{
				{"analytics", "orders", "id", "read"},
				{"analytics", "orders", "customer_id", "read"},
			},
		},
	}

	for _, tc := range cases {
		t.Run("S07/data-pat-grants-recorded-per-field", func(t *testing.T) {
			grants, err := declare.ExpandDataPATGrants(tc.reads, declared, endpoints)
			if err != nil {
				t.Fatalf("ExpandDataPATGrants: %v", err)
			}

			rec := storetest.NewWriteRecorder()
			w := store.NewWriter(rec)
			if err := w.RecordGrants(context.Background(), "iris_pat_orders", grants); err != nil {
				t.Fatalf("RecordGrants: %v", err)
			}

			if txns := rec.Transactions(); len(txns) != 1 {
				t.Fatalf("RecordGrants committed %d transactions, want 1 atomic rewrite", len(txns))
			}
			got := recordedGrantRows(t, rec, "iris_pat_orders")
			if !sameGrantSet(got, tc.want) {
				t.Errorf("%s: recorded per-field grants = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// sameGrantSet reports whether a and b hold the same grants, order-independent.
func sameGrantSet(a, b []grantRow) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[grantRow]int{}
	for _, g := range a {
		counts[g]++
	}
	for _, g := range b {
		counts[g]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
