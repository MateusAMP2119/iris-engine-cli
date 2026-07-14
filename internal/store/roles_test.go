package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store/storetest"
)

// This file proves the engine-owned access ledger's meta write surface: the
// grants and roles table shapes, the atomic write paths that populate them, and
// the credentials login-only guard. Every write rides the single meta writer over
// a recording fake -- no live Postgres -- so a test asserts the exact statement
// set and its transaction grouping.

// TestAccessLedgerShape proves the access ledger's meta shape and write path:
// grants stores (pg_role, schema, table, field, access) indexed on pg_role, and
// roles maps each pg_role to exactly one owner -- a pipeline XOR a data PAT. The
// write path registers a role with a single owner column set and replaces its
// grants as one atomic full-role rewrite.
func TestAccessLedgerShape(t *testing.T) {
	t.Run("access-ledger-shape", func(t *testing.T) {
		s := store.MetaSchema()

		// grants: (pg_role, schema, table, field, access), indexed on pg_role.
		grants := tableByName(t, s, "grants")
		for _, col := range []string{"pg_role", "schema", "table", "field", "access"} {
			columnByName(t, grants, col)
		}
		if grants.PrimaryKey[0] != "pg_role" {
			t.Errorf("grants PK does not lead with pg_role: %v", grants.PrimaryKey)
		}
		grantFK := map[string]string{}
		for _, fk := range grants.ForeignKeys {
			grantFK[fk.Column] = fk.RefTable + "." + fk.RefColumn
		}
		if grantFK["pg_role"] != "roles.pg_role" {
			t.Errorf("grants.pg_role FK = %q, want roles.pg_role", grantFK["pg_role"])
		}

		// roles: pg_role PK; owner is a pipeline XOR a data PAT (the CHECK enforces
		// exactly one set).
		roles := tableByName(t, s, "roles")
		if got := roles.PrimaryKey; len(got) != 1 || got[0] != "pg_role" {
			t.Errorf("roles PK = %v, want [pg_role]", got)
		}
		if columnByName(t, roles, "pipeline"); !columnByName(t, roles, "pipeline").Nullable {
			t.Errorf("roles.pipeline must be nullable (a data-PAT role sets pat, not pipeline)")
		}
		if !columnByName(t, roles, "pat").Nullable {
			t.Errorf("roles.pat must be nullable (a pipeline role sets pipeline, not pat)")
		}
		roleFK := map[string]string{}
		for _, fk := range roles.ForeignKeys {
			roleFK[fk.Column] = fk.RefTable + "." + fk.RefColumn
		}
		if roleFK["pipeline"] != "pipelines.name" {
			t.Errorf("roles.pipeline FK = %q, want pipelines.name", roleFK["pipeline"])
		}
		if roleFK["pat"] != "pats.id" {
			t.Errorf("roles.pat FK = %q, want pats.id", roleFK["pat"])
		}
		// The XOR CHECK: exactly one owner column is set.
		xor := false
		for _, rc := range roles.RawChecks {
			if strings.Contains(rc, "pipeline") && strings.Contains(rc, "pat") && strings.Contains(rc, "<>") {
				xor = true
			}
		}
		if !xor {
			t.Errorf("roles carries no pipeline-XOR-pat CHECK: %v", roles.RawChecks)
		}

		// Write path: a pipeline role sets the pipeline column, pat NULL.
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.RegisterRole(context.Background(), "iris_load_orders", store.PipelineOwner("load_orders")); err != nil {
			t.Fatalf("RegisterRole(pipeline): %v", err)
		}
		ins := stmtsContaining(rec.Statements(), "INSERT INTO roles")
		if len(ins) != 1 {
			t.Fatalf("RegisterRole issued %d roles inserts, want 1", len(ins))
		}
		if got := ins[0].Args; len(got) != 3 || got[0] != "iris_load_orders" || got[1] != "load_orders" || got[2] != nil {
			t.Errorf("pipeline role args = %v, want [iris_load_orders load_orders <nil>] (pat NULL)", got)
		}

		// A data-PAT role sets the pat column, pipeline NULL -- the other side of XOR.
		rec2 := storetest.NewWriteRecorder()
		w2 := store.NewWriter(rec2)
		if err := w2.RegisterRole(context.Background(), "iris_pat_orders", store.DataPATOwner("pat_7f3c")); err != nil {
			t.Fatalf("RegisterRole(data PAT): %v", err)
		}
		pat := stmtsContaining(rec2.Statements(), "INSERT INTO roles")[0]
		if got := pat.Args; len(got) != 3 || got[0] != "iris_pat_orders" || got[1] != nil || got[2] != "pat_7f3c" {
			t.Errorf("data-PAT role args = %v, want [iris_pat_orders <nil> pat_7f3c] (pipeline NULL)", got)
		}
	})
}

// TestReplaceGrantsAtomic proves ReplaceGrants records the pipeline's field-level
// grants as one atomic full-role rewrite: a clearing DELETE for the role's grants
// followed by one INSERT per grant carrying exactly (pg_role, schema, table, field,
// access), all in a single meta transaction.
func TestReplaceGrantsAtomic(t *testing.T) {
	t.Run("access-ledger-shape", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		grants := []store.Grant{
			{Schema: "analytics", Table: "orders", Field: "amount", Access: store.AccessRead},
			{Schema: "raw", Table: "orders_staging", Field: "id", Access: store.AccessWrite},
		}
		if err := w.ReplaceGrants(context.Background(), "iris_load_orders", grants); err != nil {
			t.Fatalf("ReplaceGrants: %v", err)
		}

		// One atomic transaction carrying the whole rewrite.
		txns := rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("ReplaceGrants committed %d transactions, want 1 (atomic full-role rewrite)", len(txns))
		}
		batch := txns[0]
		if len(batch) == 0 || !strings.Contains(batch[0].SQL, "DELETE FROM grants") {
			t.Errorf("atomic rewrite does not begin by clearing the role's grants: %v", batch)
		}
		inserts := stmtsContaining(batch, "INSERT INTO grants")
		if len(inserts) != 2 {
			t.Fatalf("ReplaceGrants inserted %d grant rows, want 2: %v", len(inserts), batch)
		}
		// Each row carries exactly (pg_role, schema, table, field, access).
		if got := inserts[0].Args; len(got) != 5 || got[0] != "iris_load_orders" || got[1] != "analytics" || got[2] != "orders" || got[3] != "amount" || got[4] != "read" {
			t.Errorf("read grant args = %v, want [iris_load_orders analytics orders amount read]", got)
		}
		if got := inserts[1].Args; len(got) != 5 || got[4] != "write" {
			t.Errorf("write grant args = %v, want access=write", got)
		}
		// The clearing DELETE and the inserts commit together, never split.
		if len(rec.Statements()) != len(batch) {
			t.Errorf("ReplaceGrants issued statements outside the atomic transaction: %v", rec.Statements())
		}
	})

	t.Run("empty grant set clears the role's grants and writes no row", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.ReplaceGrants(context.Background(), "iris_load_orders", nil); err != nil {
			t.Fatalf("ReplaceGrants(nil): %v", err)
		}
		if anyStmtContains(rec.Statements(), "INSERT INTO grants") {
			t.Errorf("empty grant set inserted a grant row: %v", rec.Statements())
		}
		if !anyStmtContains(rec.Statements(), "DELETE FROM grants") {
			t.Errorf("empty grant set did not clear the role's grants: %v", rec.Statements())
		}
	})
}

// TestRegisterRoleRejectsBadOwner proves RegisterRole fails loudly on an owner
// with no name or an unknown kind rather than writing a roles row that would trip
// the pipeline-XOR-pat CHECK at the database.
func TestRegisterRoleRejectsBadOwner(t *testing.T) {
	t.Run("access-ledger-shape", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		for _, owner := range []store.RoleOwner{
			store.PipelineOwner(""), // pipeline owner with no name
			store.DataPATOwner(""),  // data-PAT owner with no id
			{},                      // zero owner: neither pipeline nor pat
		} {
			if err := w.RegisterRole(context.Background(), "iris_x", owner); err == nil {
				t.Errorf("RegisterRole accepted invalid owner %+v; want a rejection", owner)
			}
		}
		if len(rec.Statements()) != 0 {
			t.Errorf("RegisterRole wrote a roles row for an invalid owner: %v", rec.Statements())
		}
	})
}

// TestCredentialsPipelineLoginOnly proves the credentials ledger holds an
// engine-managed secret per login role, and only for pipeline roles: SetCredential
// stores a row for a pipeline (login) role, and rejects a data-PAT role -- which is
// NOLOGIN and holds no credential -- writing nothing.
func TestCredentialsPipelineLoginOnly(t *testing.T) {
	t.Run("credentials-pipeline-login-only", func(t *testing.T) {
		// credentials table shape: pg_role PK, secret, FK to roles.
		s := store.MetaSchema()
		creds := tableByName(t, s, "credentials")
		if got := creds.PrimaryKey; len(got) != 1 || got[0] != "pg_role" {
			t.Errorf("credentials PK = %v, want [pg_role]", got)
		}
		columnByName(t, creds, "secret")
		credFK := map[string]string{}
		for _, fk := range creds.ForeignKeys {
			credFK[fk.Column] = fk.RefTable + "." + fk.RefColumn
		}
		if credFK["pg_role"] != "roles.pg_role" {
			t.Errorf("credentials.pg_role FK = %q, want roles.pg_role", credFK["pg_role"])
		}

		secret, err := store.GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret: %v", err)
		}

		// A pipeline (login) role gets exactly one credentials row.
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.SetCredential(context.Background(), "iris_load_orders", store.PipelineOwner("load_orders"), secret); err != nil {
			t.Fatalf("SetCredential(pipeline): %v", err)
		}
		rows := stmtsContaining(rec.Statements(), "INTO credentials")
		if len(rows) != 1 {
			t.Fatalf("SetCredential(pipeline) wrote %d credentials rows, want 1", len(rows))
		}
		if got := rows[0].Args; len(got) != 2 || got[0] != "iris_load_orders" {
			t.Errorf("credentials row args = %v, want [iris_load_orders <secret>]", got)
		}

		// A data-PAT role is NOLOGIN: SetCredential rejects it and writes nothing.
		rec2 := storetest.NewWriteRecorder()
		w2 := store.NewWriter(rec2)
		err = w2.SetCredential(context.Background(), "iris_pat_orders", store.DataPATOwner("pat_7f3c"), secret)
		if !errors.Is(err, store.ErrDataPATRoleNoCredential) {
			t.Errorf("SetCredential(data PAT) error = %v, want ErrDataPATRoleNoCredential", err)
		}
		if len(rec2.Statements()) != 0 {
			t.Errorf("SetCredential(data PAT) wrote a credentials row for a NOLOGIN role: %v", rec2.Statements())
		}
	})
}

// TestSecretNeverLeaksInFormatting proves an engine-managed credential never leaks
// through a formatting path: every fmt verb, String, and GoString redact it, so a
// stray log line can never print the secret (the AdminDSN redaction pattern).
func TestSecretNeverLeaksInFormatting(t *testing.T) {
	t.Run("credentials-pipeline-login-only", func(t *testing.T) {
		secret, err := store.GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret: %v", err)
		}
		const marker = "Secret(REDACTED)"
		for _, got := range []string{
			fmt.Sprintf("%v", secret),
			fmt.Sprintf("%s", secret),
			fmt.Sprintf("%#v", secret),
			fmt.Sprintf("%d", secret), // a numeric verb must not fall through to the struct field
			fmt.Sprintf("%+v", secret),
			secret.String(),
			secret.GoString(),
		} {
			if got != marker {
				t.Errorf("secret rendered %q, want the redacted marker %q", got, marker)
			}
		}
	})
}
