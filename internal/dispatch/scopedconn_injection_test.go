//go:build unix

package dispatch_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-lakehouse/internal/dispatch"
	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// TestScopedConnectionInjectedAtSpawn proves that each pipeline run receives an
// engine-injected scoped connection for its least-privilege Postgres role at spawn,
// and that database credentials never reach author or consumer hands:
//
//   - the engine mints the credential (store.GenerateSecret); the author supplies
//     none;
//   - the scoped connection targets the DATA database as the pipeline's least-
//     privilege role, never the admin DSN;
//   - the scoped connection redacts under every formatting verb (like Secret and
//     AdminDSN), so a stray log line can never leak the credential;
//   - at spawn the engine's scoped connection is injected as IRIS_DB_URL and WINS
//     over any IRIS_DB_URL the author declared, so the author cannot substitute a
//     connection of their own;
//   - the credential-bearing DSN never lands in a meta write (an author-reachable
//     place); it reaches only the subprocess environment.
func TestScopedConnectionInjectedAtSpawn(t *testing.T) {
	// The engine mints the credential; the author never supplies it.
	secret, err := store.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}

	role := pg.PipelineRoleName("ingest")
	scoped, err := store.BuildScopedConn(store.ScopedConnParams{
		Host:     "db.internal",
		Port:     5432,
		Database: pg.DataDatabase,
		Options:  "sslmode=disable",
	}, role, secret)
	if err != nil {
		t.Fatalf("BuildScopedConn: %v", err)
	}

	dsn := scoped.EnvValue()

	// The scoped connection targets the data database AS the pipeline role, not the
	// admin DSN: its userinfo is the least-privilege role and its path is the data
	// database.
	if !strings.HasPrefix(dsn, "postgres://"+role+":") {
		t.Errorf("scoped DSN does not authenticate as the pipeline role %q: %s", role, dsn)
	}
	if !strings.Contains(dsn, "@db.internal:5432/"+pg.DataDatabase) {
		t.Errorf("scoped DSN does not target the data database at the admin host: %s", dsn)
	}

	// Redaction: no formatting verb reveals the credential-bearing DSN; every verb
	// renders the redacted marker (the Secret / AdminDSN discipline).
	for _, verb := range []string{"%v", "%s", "%#v", "%+v", "%d"} {
		out := fmt.Sprintf(verb, scoped)
		if strings.Contains(out, dsn) || strings.Contains(out, "db.internal") {
			t.Errorf("formatting a ScopedConn with %q leaked the DSN: %s", verb, out)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("formatting a ScopedConn with %q did not redact: %s", verb, out)
		}
	}

	// At spawn, the engine's scoped connection is injected as IRIS_DB_URL and wins
	// over an IRIS_DB_URL the author tried to declare: the author cannot substitute
	// their own connection.
	h := newHarness(t)
	dir := t.TempDir()
	script := writeScript(t, dir, "probe.sh",
		"#!/bin/sh\nprintf 'DBURL=%s\\n' \"$IRIS_DB_URL\"\n")

	rh := h.start(dispatch.RunSpec{
		RunID: "r-scoped",
		Dir:   dir,
		Argv:  []string{script},
		Env:   []string{"IRIS_DB_URL=postgres://attacker:evil@untrusted/steal"},
		DBURL: dsn,
	})
	if _, err := rh.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := h.log.contents("r-scoped")
	if !strings.Contains(got, "DBURL="+dsn) {
		t.Errorf("run env IRIS_DB_URL is not the engine-injected scoped connection; got:\n%s", got)
	}
	if strings.Contains(got, "attacker") {
		t.Errorf("author-declared IRIS_DB_URL was not overridden by the engine's scoped connection; got:\n%s", got)
	}

	// The credential-bearing DSN reaches only the subprocess environment: it never
	// lands in a meta write recorded through the single writer.
	for _, s := range h.rec.Statements() {
		if strings.Contains(s.SQL, dsn) {
			t.Errorf("meta write leaked the scoped DSN in its SQL: %s", s.SQL)
		}
		for _, a := range s.Args {
			if str, ok := a.(string); ok && strings.Contains(str, dsn) {
				t.Errorf("meta write leaked the scoped DSN in a bound arg: %q", str)
			}
		}
	}
}
