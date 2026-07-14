package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pat"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store/storetest"
)

// This file proves the unified PAT store: the pats and pat_scopes table shapes,
// the atomic create path that persists a token's prefix and argon2id hash plus
// its scope rows, and the data-scope side -- an engine-managed read-only NOLOGIN
// Postgres role recorded in the access ledger, with no credentials row. Every
// write rides the single meta writer over a recording fake (no live Postgres), so
// a test asserts the exact statement set and its transaction grouping.

// TestPATStoreShape proves the pats and pat_scopes meta shapes and the atomic create
// path: pats keys a row by token prefix (id PK) with an argon2id hash, label, and
// revoked flag; pat_scopes stores one row per scope with scope in (control, read,
// data) and PK (pat_id, scope). CreatePAT persists the pats row and its scope rows as
// one atomic transaction.
func TestPATStoreShape(t *testing.T) {
	t.Run("pat-store-shape", func(t *testing.T) {
		s := store.MetaSchema()

		// pats: id PK (token prefix), hash (argon2id), label, revoked.
		pats := tableByName(t, s, "pats")
		if got := pats.PrimaryKey; len(got) != 1 || got[0] != "id" {
			t.Errorf("pats PK = %v, want [id] (the token prefix)", got)
		}
		for _, col := range []string{"id", "hash", "label", "revoked"} {
			columnByName(t, pats, col)
		}
		if columnByName(t, pats, "revoked").Type != "boolean" {
			t.Errorf("pats.revoked type = %q, want boolean", columnByName(t, pats, "revoked").Type)
		}

		// pat_scopes: one row per scope, PK (pat_id, scope), scope CHECK (control,
		// read, data), FK to pats.
		scopes := tableByName(t, s, "pat_scopes")
		if got := scopes.PrimaryKey; len(got) != 2 || got[0] != "pat_id" || got[1] != "scope" {
			t.Errorf("pat_scopes PK = %v, want [pat_id scope]", got)
		}
		scopeFK := map[string]string{}
		for _, fk := range scopes.ForeignKeys {
			scopeFK[fk.Column] = fk.RefTable + "." + fk.RefColumn
		}
		if scopeFK["pat_id"] != "pats.id" {
			t.Errorf("pat_scopes.pat_id FK = %q, want pats.id", scopeFK["pat_id"])
		}
		var scopeCheck []string
		for _, ck := range scopes.Checks {
			if ck.Column == "scope" {
				scopeCheck = ck.Values
			}
		}
		if strings.Join(scopeCheck, ",") != "control,read,data" {
			t.Errorf("pat_scopes.scope CHECK = %v, want [control read data]", scopeCheck)
		}

		// Write path: CreatePAT persists the pats row and one pat_scopes row per scope,
		// as one atomic transaction.
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.CreatePAT(context.Background(), store.PATRecord{
			ID:     "0a1b2c3d",
			Hash:   "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA",
			Label:  "ci-bot",
			Scopes: []string{"control", "read"},
		}); err != nil {
			t.Fatalf("CreatePAT: %v", err)
		}

		txns := rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("CreatePAT committed %d transactions, want 1 (atomic pats + pat_scopes)", len(txns))
		}
		batch := txns[0]

		patIns := stmtsContaining(batch, "INSERT INTO pats")
		if len(patIns) != 1 {
			t.Fatalf("CreatePAT issued %d pats inserts, want 1", len(patIns))
		}
		// pats row carries prefix, hash, label, revoked=false.
		if got := patIns[0].Args; len(got) != 4 || got[0] != "0a1b2c3d" || got[1] != "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA" || got[2] != "ci-bot" || got[3] != false {
			t.Errorf("pats insert args = %v, want [0a1b2c3d <hash> ci-bot false]", got)
		}
		scopeIns := stmtsContaining(batch, "INSERT INTO pat_scopes")
		if len(scopeIns) != 2 {
			t.Fatalf("CreatePAT issued %d pat_scopes inserts, want 2", len(scopeIns))
		}
		if got := scopeIns[0].Args; len(got) != 2 || got[0] != "0a1b2c3d" || got[1] != "control" {
			t.Errorf("first pat_scopes insert args = %v, want [0a1b2c3d control]", got)
		}
		if got := scopeIns[1].Args; got[1] != "read" {
			t.Errorf("second pat_scopes scope = %v, want read", got[1])
		}

		// The whole create is one transaction: no statement escaped it.
		if len(rec.Statements()) != len(batch) {
			t.Errorf("CreatePAT issued statements outside the atomic transaction: %v", rec.Statements())
		}
	})
}

// TestCreatePATRejectsEmptyInputs proves CreatePAT fails loudly on a missing prefix,
// hash, or scope set rather than writing a half-formed PAT row (a PAT with no hash
// could never authenticate; a PAT with no scope gates nothing).
func TestCreatePATRejectsEmptyInputs(t *testing.T) {
	t.Run("pat-store-shape", func(t *testing.T) {
		for _, rec := range []store.PATRecord{
			{ID: "", Hash: "h", Label: "l", Scopes: []string{"read"}},
			{ID: "id", Hash: "", Label: "l", Scopes: []string{"read"}},
			{ID: "id", Hash: "h", Label: "l", Scopes: nil},
		} {
			wr := storetest.NewWriteRecorder()
			w := store.NewWriter(wr)
			if err := w.CreatePAT(context.Background(), rec); err == nil {
				t.Errorf("CreatePAT accepted an invalid record %+v; want a rejection", rec)
			}
			if len(wr.Statements()) != 0 {
				t.Errorf("CreatePAT wrote a row for an invalid record %+v: %v", rec, wr.Statements())
			}
		}
	})
}

// TestDataPATOwnsReadRole proves granting a PAT the data scope records an
// engine-managed read-only Postgres role for it in the access ledger (roles),
// owner=data-PAT, together with its fixed field-level read grants, all in the same
// atomic transaction that writes the pats and pat_scopes rows.
func TestDataPATOwnsReadRole(t *testing.T) {
	t.Run("data-pat-owns-read-role", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		role := pat.DataRoleName("0a1b2c3d")
		err := w.CreatePAT(context.Background(), store.PATRecord{
			ID:       "0a1b2c3d",
			Hash:     "$argon2id$v=19$m=65536,t=1,p=4$c2FsdA$aGFzaA",
			Label:    "reader",
			Scopes:   []string{"data"},
			DataRole: role,
			DataGrants: []store.Grant{
				{Schema: "analytics", Table: "orders", Field: "amount", Access: store.AccessRead},
			},
		})
		if err != nil {
			t.Fatalf("CreatePAT(data): %v", err)
		}

		txns := rec.Transactions()
		if len(txns) != 1 {
			t.Fatalf("data-PAT create committed %d transactions, want 1 (atomic PAT + role + grants)", len(txns))
		}
		batch := txns[0]

		// The role is recorded owner=data-PAT: roles.pat set to the token id, pipeline NULL.
		roleIns := stmtsContaining(batch, "INSERT INTO roles")
		if len(roleIns) != 1 {
			t.Fatalf("data-PAT create issued %d roles inserts, want 1", len(roleIns))
		}
		if got := roleIns[0].Args; len(got) != 3 || got[0] != role || got[1] != nil || got[2] != "0a1b2c3d" {
			t.Errorf("data-PAT role args = %v, want [%s <nil> 0a1b2c3d] (pipeline NULL, pat set)", got, role)
		}

		// Its read grants are recorded in the same transaction.
		grantIns := stmtsContaining(batch, "INSERT INTO grants")
		if len(grantIns) != 1 {
			t.Fatalf("data-PAT create issued %d grant inserts, want 1", len(grantIns))
		}
		if got := grantIns[0].Args; len(got) != 5 || got[0] != role || got[1] != "analytics" || got[2] != "orders" || got[3] != "amount" || got[4] != "read" {
			t.Errorf("data-PAT grant args = %v, want [%s analytics orders amount read]", got, role)
		}
	})
}

// TestDataPATRoleNoLoginSetRole proves a data-PAT role is created NOLOGIN and is the
// kind assumed via SET ROLE on the API read path, not a login role: its ledger owner
// is a data PAT (IsLogin false), and the create path writes no credentials row. The
// read-path SET ROLE itself lands in a later route task; here the role shape is what
// makes it assumable.
func TestDataPATRoleNoLoginSetRole(t *testing.T) {
	t.Run("data-pat-role-nologin-set-role", func(t *testing.T) {
		// The ledger owner of a data-PAT role is NOLOGIN by construction: a data PAT
		// owner is not a login owner (pipeline roles log in and hold a credential).
		owner := store.DataPATOwner("0a1b2c3d")
		if owner.IsLogin() {
			t.Errorf("data-PAT role owner reports IsLogin true; data-PAT roles are NOLOGIN")
		}

		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		role := pat.DataRoleName("0a1b2c3d")
		if err := w.CreatePAT(context.Background(), store.PATRecord{
			ID:         "0a1b2c3d",
			Hash:       "$argon2id$hash",
			Label:      "reader",
			Scopes:     []string{"data"},
			DataRole:   role,
			DataGrants: []store.Grant{{Schema: "analytics", Table: "orders", Field: "amount", Access: store.AccessRead}},
		}); err != nil {
			t.Fatalf("CreatePAT(data): %v", err)
		}

		// No credentials row is ever written for a data-PAT role (it is NOLOGIN).
		if anyStmtContains(rec.Statements(), "INTO credentials") {
			t.Errorf("data-PAT create wrote a credentials row; a NOLOGIN role holds none: %v", rec.Statements())
		}
	})
}

// TestDataPATRoleNoLoginNoCredentials proves a data PAT maps to an engine-managed
// read-only NOLOGIN role assumed via SET ROLE and gets no row in credentials, which
// holds pipeline login roles only. The create path writes none, and the ledger's
// own guard rejects a credential for a data-PAT role.
func TestDataPATRoleNoLoginNoCredentials(t *testing.T) {
	t.Run("data-pat-role-nologin-no-credentials", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		role := pat.DataRoleName("0a1b2c3d")
		if err := w.CreatePAT(context.Background(), store.PATRecord{
			ID:         "0a1b2c3d",
			Hash:       "$argon2id$hash",
			Label:      "reader",
			Scopes:     []string{"data"},
			DataRole:   role,
			DataGrants: []store.Grant{{Schema: "analytics", Table: "orders", Field: "amount", Access: store.AccessRead}},
		}); err != nil {
			t.Fatalf("CreatePAT(data): %v", err)
		}
		if anyStmtContains(rec.Statements(), "credentials") {
			t.Errorf("data-PAT create touched credentials; credentials holds pipeline login roles only: %v", rec.Statements())
		}

		// The ledger guard is explicit: a credential for a data-PAT (NOLOGIN) role is
		// refused, so no code path can slip one in later.
		secret, err := store.GenerateSecret()
		if err != nil {
			t.Fatalf("GenerateSecret: %v", err)
		}
		err = w.SetCredential(context.Background(), role, store.DataPATOwner("0a1b2c3d"), secret)
		if !errors.Is(err, store.ErrDataPATRoleNoCredential) {
			t.Errorf("SetCredential(data-PAT role) = %v, want ErrDataPATRoleNoCredential", err)
		}
	})
}

// TestPATShowOnceHash proves the persistence half of show-once: composing the
// real pat mint with the store, PAT creation persists only the token prefix and
// its argon2id hash (plus label and scopes) -- never the raw token. The raw token
// appears in no persisted argument, and the stored hash verifies the token it was
// minted from, so a lost token can only be revoked and re-minted, never recovered
// from meta.
func TestPATShowOnceHash(t *testing.T) {
	t.Run("pat-show-once-hash", func(t *testing.T) {
		tok, err := pat.Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		hash, err := pat.Hash(tok)
		if err != nil {
			t.Fatalf("Hash: %v", err)
		}

		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.CreatePAT(context.Background(), store.PATRecord{
			ID:     tok.ID(),
			Hash:   hash,
			Label:  "ci-bot",
			Scopes: []string{"control"},
		}); err != nil {
			t.Fatalf("CreatePAT: %v", err)
		}

		full := tok.Reveal()
		// Persistence records only the prefix and hash: the raw show-once token appears
		// in no recorded statement or argument.
		for _, st := range rec.Statements() {
			if strings.Contains(st.SQL, full) {
				t.Errorf("persisted SQL leaked the raw token: %q", st.SQL)
			}
			for _, a := range st.Args {
				if s, ok := a.(string); ok && strings.Contains(s, full) {
					t.Errorf("persisted argument leaked the raw token: %q", s)
				}
			}
		}

		// The prefix and hash are what got stored.
		patIns := stmtsContaining(rec.Statements(), "INSERT INTO pats")
		if len(patIns) != 1 || patIns[0].Args[0] != tok.ID() || patIns[0].Args[1] != hash {
			t.Fatalf("pats insert = %v, want the prefix %q and argon2id hash", patIns, tok.ID())
		}

		// The stored hash still verifies the token it was minted from (a lost token is
		// revoked + re-minted, never recovered from this one-way hash).
		ok, err := pat.Verify(full, hash)
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if !ok {
			t.Errorf("stored hash does not verify the minted token")
		}
	})
}

// TestRevokePAT proves a PAT is revoked by flipping its pats.revoked flag by prefix,
// a single guarded statement -- the disposition for a lost token (revoke + re-mint).
func TestRevokePAT(t *testing.T) {
	t.Run("pat-show-once-hash", func(t *testing.T) {
		rec := storetest.NewWriteRecorder()
		w := store.NewWriter(rec)
		if err := w.RevokePAT(context.Background(), "0a1b2c3d"); err != nil {
			t.Fatalf("RevokePAT: %v", err)
		}
		stmts := stmtsContaining(rec.Statements(), "UPDATE pats")
		if len(stmts) != 1 {
			t.Fatalf("RevokePAT issued %d pats updates, want 1", len(stmts))
		}
		if !strings.Contains(stmts[0].SQL, "revoked") {
			t.Errorf("RevokePAT does not set the revoked flag: %q", stmts[0].SQL)
		}
		if got := stmts[0].Args; len(got) != 1 || got[0] != "0a1b2c3d" {
			t.Errorf("RevokePAT args = %v, want [0a1b2c3d]", got)
		}

		if err := w.RevokePAT(context.Background(), ""); err == nil {
			t.Errorf("RevokePAT accepted an empty prefix")
		}
	})
}
