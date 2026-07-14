package declare_test

import (
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file proves data-PAT grant resolution at mint: the three --read/--endpoint
// shapes a data PAT's read grants resolve from, each recorded per field so a column
// added after mint is never silently granted. It reuses the declare leaf's
// ExpandDataPATGrants, the pure resolver the leader drives at mint. Endpoint
// expansion consumes a supplied endpoints map, which the daemon's mint path builds
// from the live endpoint registry (hydrated from the persisted endpoints at startup,
// republished by each endpoint apply); here the map stands in.

// grantKey renders a FieldGrant as a comparable "schema.table.field:access" string.
func grantKey(g declare.FieldGrant) string {
	return g.Schema + "." + g.Table + "." + g.Field + ":" + string(g.Access)
}

// grantKeys renders a grant set as a set of keys for order-independent membership.
func grantKeys(gs []declare.FieldGrant) map[string]bool {
	out := make(map[string]bool, len(gs))
	for _, g := range gs {
		out[grantKey(g)] = true
	}
	return out
}

// TestDataPATGrantResolution proves the three data-PAT read shapes resolve at mint:
// --read field-explicit grants that one field, a bare schema.table expands to every
// field declared at that moment (recorded per field), and --endpoint expands to the
// endpoint's source fields. Every grant is read-only, and a column added after mint is
// never silently granted (the recorded set is fixed to the fields declared at mint).
func TestDataPATGrantResolution(t *testing.T) {
	t.Run("data-pat-grant-resolution", func(t *testing.T) {
		// The declared world at mint time: analytics.orders declares three fields.
		declaredFields := map[string][]string{
			"analytics.orders": {"id", "amount", "customer"},
		}
		// An endpoint's persisted source and projection (at mint the daemon supplies
		// this map from the live endpoint registry).
		endpoints := map[string]declare.EndpointSource{
			"orders_by_customer": {Source: "analytics.orders", Fields: []string{"id", "customer"}},
		}

		t.Run("field-explicit grants that one field", func(t *testing.T) {
			got, err := declare.ExpandDataPATGrants(
				[]declare.DataPATRead{{Table: "analytics.orders", Field: "amount"}},
				declaredFields, endpoints)
			if err != nil {
				t.Fatalf("ExpandDataPATGrants: %v", err)
			}
			keys := grantKeys(got)
			if len(keys) != 1 || !keys["analytics.orders.amount:read"] {
				t.Errorf("field-explicit read = %v, want just analytics.orders.amount:read", keys)
			}
		})

		t.Run("bare schema.table expands to all declared fields, recorded per field", func(t *testing.T) {
			got, err := declare.ExpandDataPATGrants(
				[]declare.DataPATRead{{Table: "analytics.orders"}},
				declaredFields, endpoints)
			if err != nil {
				t.Fatalf("ExpandDataPATGrants: %v", err)
			}
			keys := grantKeys(got)
			for _, f := range []string{"id", "amount", "customer"} {
				if !keys["analytics.orders."+f+":read"] {
					t.Errorf("bare table did not grant field %q per field; got %v", f, keys)
				}
			}
			if len(keys) != 3 {
				t.Errorf("bare table granted %d fields, want the 3 declared at mint", len(keys))
			}

			// A column added after mint is never silently granted: the recorded set is
			// fixed to the fields declared at mint. A column not declared at mint is
			// absent from the resolved set, so a later ALTER that adds it cannot
			// retroactively widen this PAT's grants.
			if keys["analytics.orders.added_later:read"] {
				t.Errorf("mint-time grant set contains a column not declared at mint: %v", keys)
			}
		})

		t.Run("endpoint expands to the endpoint's source fields", func(t *testing.T) {
			got, err := declare.ExpandDataPATGrants(
				[]declare.DataPATRead{{Endpoint: "orders_by_customer"}},
				declaredFields, endpoints)
			if err != nil {
				t.Fatalf("ExpandDataPATGrants: %v", err)
			}
			keys := grantKeys(got)
			if len(keys) != 2 || !keys["analytics.orders.id:read"] || !keys["analytics.orders.customer:read"] {
				t.Errorf("endpoint read = %v, want the endpoint's source fields id, customer", keys)
			}
		})

		t.Run("an unknown table or endpoint is rejected, naming it", func(t *testing.T) {
			if _, err := declare.ExpandDataPATGrants(
				[]declare.DataPATRead{{Table: "analytics.nope"}}, declaredFields, endpoints); err == nil {
				t.Errorf("bare read of an undeclared table was accepted")
			}
			if _, err := declare.ExpandDataPATGrants(
				[]declare.DataPATRead{{Endpoint: "ghost"}}, declaredFields, endpoints); err == nil {
				t.Errorf("read of an unknown endpoint was accepted")
			}
		})
	})
}
