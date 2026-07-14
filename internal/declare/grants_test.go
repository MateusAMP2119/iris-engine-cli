package declare_test

import (
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
)

// This file proves the grant-intent leaf logic: the standing bounds every
// engine-managed role is held to, expansion of a
// declaration's reads/writes into per-field grants, and expansion of a data PAT's
// mint read specs. It is pure over values, no I/O.

// TestStrayPublicGrantNonAdditive proves the public-bounds rule specifically: a
// live grant exceeding the standing bounds every pipeline and data-PAT role is
// held to -- on public a role may hold read (SELECT) only, and no role may CONNECT
// to meta -- is classified non-additive drift, reported, and never silently fixed.
// ExceedsStandingBounds codifies the rule, and
// ClassifyGrantDrift reports such a grant as a stray when it is beyond the ledger's
// bounds.
func TestStrayPublicGrantNonAdditive(t *testing.T) {
	t.Run("stray-public-grant-nonadditive", func(t *testing.T) {
		// ExceedsStandingBounds codifies the two standing invariants.
		beyond := []declare.Grant{
			{Role: "iris_pat_orders", Schema: "public", Object: "orders", Privilege: "INSERT"},
			{Role: "iris_pat_orders", Schema: "public", Object: "orders", Privilege: "UPDATE"},
			{Role: "iris_pat_orders", Schema: "public", Object: "orders", Privilege: "DELETE"},
			{Role: "iris_pat_orders", Schema: "meta", Object: "meta", Privilege: "CONNECT"},
		}
		for _, g := range beyond {
			if exceeds, reason := declare.ExceedsStandingBounds(g); !exceeds {
				t.Errorf("ExceedsStandingBounds(%s %s.%s) = false, want true (beyond the standing bounds)", g.Privilege, g.Schema, g.Object)
			} else if reason == "" {
				t.Errorf("ExceedsStandingBounds(%s %s.%s) returned no reason", g.Privilege, g.Schema, g.Object)
			}
		}

		// Within bounds: SELECT on public (the readable journal surface) and SELECT
		// on a declared user schema are both allowed and never flagged.
		within := []declare.Grant{
			{Role: "iris_pat_orders", Schema: "public", Object: "data_journal", Privilege: "SELECT"},
			{Role: "iris_pat_orders", Schema: "analytics", Object: "orders", Privilege: "SELECT"},
		}
		for _, g := range within {
			if exceeds, _ := declare.ExceedsStandingBounds(g); exceeds {
				t.Errorf("ExceedsStandingBounds(%s %s.%s) = true, want false (within bounds)", g.Privilege, g.Schema, g.Object)
			}
		}

		// A live grant beyond the ledger's read-only-on-public bounds rides the drift
		// report as non-additive, reported, never autofixed. Bounds assert only read
		// on public; the live set adds a write on public and a CONNECT to meta.
		bounds := []declare.Grant{
			{Role: "iris_pat_orders", Schema: "public", Object: "data_journal", Privilege: "SELECT"},
		}
		report := declare.ClassifyGrantDrift(declare.GrantView{
			Bounds: bounds,
			Live: append([]declare.Grant{
				{Role: "iris_pat_orders", Schema: "public", Object: "orders", Privilege: "INSERT"},
				{Role: "iris_pat_orders", Schema: "meta", Object: "meta", Privilege: "CONNECT"},
			}, bounds...),
		})

		strays := report.NonAdditive()
		if len(strays) != 2 {
			t.Fatalf("ClassifyGrantDrift reported %d strays, want 2 (the write-on-public and the meta CONNECT): %+v", len(strays), strays)
		}
		for _, d := range strays {
			if d.Kind != declare.DriftNonAdditive {
				t.Errorf("stray %q kind = %q, want non_additive", d.Name, d.Kind)
			}
			if d.Action != declare.ActionReport {
				t.Errorf("stray %q action = %q, want report (never silently fixed)", d.Name, d.Action)
			}
		}
		// No stray is ever offered an autofix -- the whole point of "never silently
		// fixed".
		if fixes := report.Autofixes(); len(fixes) != 0 {
			t.Errorf("a stray-beyond-bounds grant was offered an autofix: %+v", fixes)
		}
	})
}
