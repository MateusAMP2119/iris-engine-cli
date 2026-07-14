//go:build conformance

package conformance

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// TestJournalSelectOnly stands up a real Postgres cluster the engine has never
// touched, ensures the data database and provisions the partitioned
// public.data_journal (parent, its initial tail partition, and the select grant)
// through the live pg path, then connects to the cluster AS a non-owner login
// role to prove one enforcement contract against the live database:
//
//   - Every engine role may SELECT public.data_journal (the grant is TO PUBLIC,
//     so present and future roles read it), and no non-owner role may write it --
//     an INSERT as the non-owner fails at Postgres with insufficient_privilege.
//     The journal's owner (as its capture triggers do) writes it, proving the
//     table is functional and the restriction is a privilege boundary, not a
//     broken table.
//
// It drives the pg journal DDL directly against the live cluster (the
// external_data_db conformance pattern) rather than the CLI, so the leg proves the
// DDL the engine issues, enforced by a real Postgres. The managed embedded-postgres
// runtime is cached after the first run; the leg reports its own wall time.
func TestJournalSelectOnly(t *testing.T) {
	start := time.Now()
	t.Cleanup(func() { t.Logf("journal select-only conformance leg: %s", time.Since(start).Round(time.Millisecond)) })

	const (
		superuser = "postgres"
		superpw   = "superpw"
		reader    = "iris_journal_reader"
		readerpw  = "reader_pw"
	)
	port := freePort(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	runtimeDir := filepath.Join(t.TempDir(), "runtime")

	cluster := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V18).
		Username(superuser).Password(superpw).Database("postgres").
		Port(port).
		DataPath(dataDir).RuntimePath(runtimeDir).
		StartTimeout(90 * time.Second))
	if err := cluster.Start(); err != nil {
		t.Fatalf("start bare Postgres cluster: %v", err)
	}
	t.Cleanup(func() { _ = cluster.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dsnTo := func(db, user, pw string) string {
		return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable", user, pw, port, db)
	}

	// The data-database admin client, exactly as the engine opens it: pg.Connect
	// ensures the engine-owned data database and returns a client on it.
	client, err := pg.Connect(ctx, testConnSource{dsn: dsnTo("postgres", superuser, superpw)})
	if err != nil {
		t.Fatalf("pg.Connect (data database): %v", err)
	}
	t.Cleanup(client.Close)

	// Provision the journal live: the partitioned parent and its provenance index,
	// its initial (open, unsealed) tail partition so the table is writable, and the
	// select grant that opens SELECT to every role.
	if err := pg.EnsureJournal(ctx, client); err != nil {
		t.Fatalf("EnsureJournal: %v", err)
	}
	if err := client.Exec(ctx, pg.InitialPartition().CreateDDL()); err != nil {
		t.Fatalf("create initial journal partition: %v", err)
	}
	if err := client.Exec(ctx, pg.JournalSelectGrantDDL()); err != nil {
		t.Fatalf("apply journal select grant: %v", err)
	}

	// Mint a non-owner login role -- not a superuser, not the journal's owner.
	for _, stmt := range []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD '%s'", reader, readerpw),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", pg.DataDatabase, reader),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", reader),
	} {
		if err := client.Exec(ctx, stmt); err != nil {
			t.Fatalf("mint non-owner reader role (%q): %v", stmt, err)
		}
	}

	// The owner writes the journal (as the capture triggers do), proving the table
	// is functional -- one row lands in the tail partition.
	if err := client.Exec(ctx, `INSERT INTO public.data_journal
		(pg_role, run_id, "schema", "table", row_pk, op, undo, recorded_at)
		VALUES ('iris_pipeline_load_orders', 42, 'analytics', 'orders', '9f3c', 'insert', 'open', 'log-1')`); err != nil {
		t.Fatalf("owner insert into data_journal failed; the table is not writable by its owner: %v", err)
	}

	readerConn, err := pgx.Connect(ctx, dsnTo(pg.DataDatabase, reader, readerpw))
	if err != nil {
		t.Fatalf("connect as the non-owner reader role: %v", err)
	}
	defer func() { _ = readerConn.Close(ctx) }()

	// Every role may SELECT: the reader reads the journal (and sees the owner's row)
	// though it was never granted SELECT explicitly -- the grant is TO PUBLIC.
	var n int
	if err := readerConn.QueryRow(ctx, "SELECT count(*) FROM public.data_journal").Scan(&n); err != nil {
		t.Fatalf("non-owner SELECT of public.data_journal was refused; every role may read the journal: %v", err)
	}
	if n != 1 {
		t.Errorf("non-owner read saw %d journal rows, want 1 (the owner's write)", n)
	}

	// No role may write: an INSERT as the non-owner is refused by Postgres with
	// insufficient_privilege -- writes reach the journal only through its owner.
	_, err = readerConn.Exec(ctx, `INSERT INTO public.data_journal
		(pg_role, run_id, "schema", "table", row_pk, op, undo, recorded_at)
		VALUES ('iris_journal_reader', 99, 'analytics', 'orders', 'dead', 'insert', 'open', 'log-2')`)
	if err == nil {
		t.Fatal("non-owner INSERT into public.data_journal succeeded; no role may write the journal")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("non-owner INSERT failed but not with a Postgres error: %v", err)
	}
	if pgErr.Code != insufficientPrivilege {
		t.Errorf("non-owner INSERT denied with SQLSTATE %s, want %s (insufficient_privilege): %v",
			pgErr.Code, insufficientPrivilege, err)
	}
}
