package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file proves the leader's grant-drift pass: a ledgered grant Postgres
// lost is re-issued (the ledger is authoritative), a stray grant Postgres holds
// beyond the ledger is reported and never revoked, and an unwired seam is a
// no-op.

// driftLedgerFake scripts the meta ledger read.
type driftLedgerFake struct{ rows []store.RoleGrantLedger }

func (f driftLedgerFake) DataPATRoleGrants(context.Context) ([]store.RoleGrantLedger, error) {
	return f.rows, nil
}

// driftDataFake is a dataPlane whose live grants are scripted per role and whose
// Exec records the DDL the reconcile issues.
type driftDataFake struct {
	controlDataFake
	live  map[string][]declare.FieldGrant
	execs []string
}

func (f *driftDataFake) Exec(_ context.Context, sql string) error {
	f.execs = append(f.execs, sql)
	return nil
}

func (f *driftDataFake) ReadFieldGrants(_ context.Context, role string) ([]declare.FieldGrant, error) {
	return f.live[role], nil
}

func TestGrantDriftReconcile(t *testing.T) {
	t.Run("grant-drift-reconcile", func(t *testing.T) {
		ctx := context.Background()
		read := func(field string) declare.FieldGrant {
			return declare.FieldGrant{Schema: "analytics", Table: "orders", Field: field, Access: declare.AccessRead}
		}

		t.Run("a lost ledgered grant is re-issued and a stray is reported, never revoked", func(t *testing.T) {
			var logBuf bytes.Buffer
			data := &driftDataFake{live: map[string][]declare.FieldGrant{
				// The role lost its ledgered "amount" read and gained a stray "secret" read.
				"iris_pat_a": {read("secret")},
			}}
			c := &Candidate{
				data:           data,
				patGrantLedger: driftLedgerFake{rows: []store.RoleGrantLedger{{Role: "iris_pat_a", Grants: []declare.FieldGrant{read("amount")}}}},
				logger:         slog.New(slog.NewTextHandler(&logBuf, nil)),
			}
			c.reconcileGrantDrift(ctx)

			// The additive half: the lost ledgered grant was re-issued as GRANT DDL.
			if len(data.execs) != 1 || !strings.Contains(data.execs[0], `SELECT ("amount")`) {
				t.Errorf("re-issued DDL = %v, want one GRANT SELECT (\"amount\")", data.execs)
			}
			// No REVOKE ever: strays are reported only.
			for _, ddl := range data.execs {
				if strings.Contains(ddl, "REVOKE") {
					t.Errorf("drift pass issued a REVOKE (%q); strays are reported, never revoked", ddl)
				}
			}
			logged := logBuf.String()
			if !strings.Contains(logged, "STRAY") || !strings.Contains(logged, "iris_pat_a") {
				t.Errorf("stray grant was not reported in the log:\n%s", logged)
			}
		})

		t.Run("a role matching its ledger issues nothing and reports nothing", func(t *testing.T) {
			var logBuf bytes.Buffer
			data := &driftDataFake{live: map[string][]declare.FieldGrant{"iris_pat_b": {read("amount")}}}
			c := &Candidate{
				data:           data,
				patGrantLedger: driftLedgerFake{rows: []store.RoleGrantLedger{{Role: "iris_pat_b", Grants: []declare.FieldGrant{read("amount")}}}},
				logger:         slog.New(slog.NewTextHandler(&logBuf, nil)),
			}
			c.reconcileGrantDrift(ctx)
			if len(data.execs) != 0 {
				t.Errorf("a drift-free role issued DDL: %v", data.execs)
			}
			if strings.Contains(logBuf.String(), "STRAY") {
				t.Errorf("a drift-free role reported a stray:\n%s", logBuf.String())
			}
		})

		t.Run("an unwired seam is a no-op", func(_ *testing.T) {
			c := &Candidate{logger: discardLogger()}
			c.reconcileGrantDrift(ctx) // must not panic and must touch nothing
		})
	})
}
