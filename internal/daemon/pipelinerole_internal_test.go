package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/declare"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the apply-time pipeline-role provisioning: a pipeline apply
// provisions the least-privilege login role on the data database (exactly the
// declaration's field grants), records the role/grants/credential in the meta
// access ledger through the single writer, and reuses the persisted credential
// on a re-apply (create-once) instead of minting a fresh secret.

func pipelineDecl(name string) *declare.Pipeline {
	return &declare.Pipeline{
		Name:   name,
		Reads:  []declare.Access{{Table: "analytics.orders", Fields: []string{"id"}}},
		Writes: []declare.Access{{Table: "analytics.rollup", Fields: []string{"total"}}},
	}
}

// declaredLive is a live view where the declaration's tables exist.
func declaredLive() pg.LiveView {
	return pg.LiveView{Tables: map[string]bool{"analytics.orders": true, "analytics.rollup": true}}
}

func TestApplyProvisionsPipelineRole(t *testing.T) {
	t.Run("apply-provisions-pipeline-role", func(t *testing.T) {
		ctx := context.Background()

		t.Run("first apply provisions the role and records the full ledger", func(t *testing.T) {
			rec := storetest.NewWriteRecorder()
			data := &driftDataFake{}
			data.controlDataFake.live = declaredLive()
			o := &controlOrchestrator{
				data:      data,
				submit:    gateSubmitter{rec: rec},
				roleCreds: credsFake{secrets: nil},
				logger:    discardLogger(),
			}
			if err := o.provisionPipelineRole(ctx, pipelineDecl("load")); err != nil {
				t.Fatalf("provisionPipelineRole: %v", err)
			}

			// The data-database DDL: role creation, credential, meta CONNECT revoke,
			// and the declared field grants -- nothing else grants access.
			ddl := strings.Join(data.execs, "\n")
			for _, want := range []string{
				"CREATE ROLE", "iris_pipeline_load", "REVOKE CONNECT", `SELECT ("id")`, `("total")`,
			} {
				if !strings.Contains(ddl, want) {
					t.Errorf("provisioning DDL missing %q:\n%s", want, ddl)
				}
			}

			// The meta ledger: the role row, its grants, and the credential, all
			// through the single writer.
			stmts := rec.Statements()
			var roles, grants, creds int
			for _, s := range stmts {
				switch {
				case strings.Contains(s.SQL, "INSERT INTO roles"):
					roles++
				case strings.Contains(s.SQL, "INSERT INTO grants"):
					grants++
				case strings.Contains(s.SQL, "INSERT INTO credentials"):
					creds++
				}
			}
			if roles == 0 || grants == 0 || creds == 0 {
				t.Errorf("ledger writes = roles %d, grants %d, credentials %d; want all three recorded", roles, grants, creds)
			}
		})

		t.Run("a re-apply reuses the persisted credential (create-once)", func(t *testing.T) {
			rec := storetest.NewWriteRecorder()
			data := &driftDataFake{}
			data.controlDataFake.live = declaredLive()
			o := &controlOrchestrator{
				data:      data,
				submit:    gateSubmitter{rec: rec},
				roleCreds: newCredsFake(t, "iris_pipeline_load"),
				logger:    discardLogger(),
			}
			if err := o.provisionPipelineRole(ctx, pipelineDecl("load")); err != nil {
				t.Fatalf("provisionPipelineRole (re-apply): %v", err)
			}
			for _, s := range rec.Statements() {
				if strings.Contains(s.SQL, "INSERT INTO credentials") {
					t.Errorf("a re-apply re-minted the credential; the persisted secret must be reused")
				}
			}
			// The idempotent DDL still runs (role ensure + grants), on the persisted secret.
			if len(data.execs) == 0 {
				t.Error("a re-apply issued no provisioning DDL; the idempotent re-provision must run")
			}
		})

		t.Run("grants on absent tables are deferred, the role still provisions", func(t *testing.T) {
			rec := storetest.NewWriteRecorder()
			data := &driftDataFake{} // empty live view: no declared table exists yet
			o := &controlOrchestrator{
				data:      data,
				submit:    gateSubmitter{rec: rec},
				roleCreds: credsFake{secrets: nil},
				logger:    discardLogger(),
			}
			if err := o.provisionPipelineRole(ctx, pipelineDecl("early")); err != nil {
				t.Fatalf("provisionPipelineRole over absent tables: %v", err)
			}
			ddl := strings.Join(data.execs, "\n")
			if !strings.Contains(ddl, "CREATE ROLE") || !strings.Contains(ddl, "REVOKE CONNECT") {
				t.Errorf("role/boundary DDL missing despite deferred grants:\n%s", ddl)
			}
			if strings.Contains(ddl, `SELECT ("id")`) || strings.Contains(ddl, `("total")`) {
				t.Errorf("grants were issued against absent tables:\n%s", ddl)
			}
		})

		t.Run("unwired seams skip provisioning", func(t *testing.T) {
			o := &controlOrchestrator{logger: discardLogger()}
			if err := o.provisionPipelineRole(ctx, pipelineDecl("shape")); err != nil {
				t.Fatalf("unwired provisioning errored: %v", err)
			}
		})
	})
}
