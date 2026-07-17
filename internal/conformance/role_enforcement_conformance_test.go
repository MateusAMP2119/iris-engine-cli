//go:build conformance

package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// This file is the declared-access enforcement leg under the turn protocol
// (#206). Its predecessor proved a real Postgres bounded each run's own
// connection to its declared columns; runs no longer have a connection to
// bound -- pipelines hold no database credentials, and every output row rides
// a stdout row frame. The successor contract this file proves end to end: the
// ENGINE checks each row frame against the declared writes, and a frame naming
// an undeclared table or an undeclared field of a declared table dead-letters
// the run as a protocol violation -- reason failed, cause loop (the fresh turn
// mints its run directly dead-lettered, exit code NULL), the offending line
// quoted verbatim in the dead letter's error -- and commits NOTHING: the
// turn's data transaction is one atomic commit, so even the in-declaration row
// framed earlier in the same turn never lands, and the journal stays empty.

// otherSchemaYAML declares testdata.other: a real, provisioned table the
// enforcement pipeline never declares, so the refusal to write it is
// declared-access enforcement, not a missing table.
const otherSchemaYAML = "schema: testdata\ntable: other\ncolumns:\n  - name: id\n    type: int\n    primary_key: true\n"

func TestEngineEnforcesDeclaredWrites(t *testing.T) {
	cases := []struct {
		name      string
		writes    string // the declaration's writes block
		inside    string // an in-declaration emit issued before the violation
		offending string // the raw row frame outside the declared writes
		wantCause string // the violation cause the dead letter must name
	}{
		{
			name:      "undeclared-table-dead-letters-and-commits-nothing",
			writes:    "writes:\n  - table: testdata.items\n    fields: [id, val]\n",
			inside:    `emit("testdata.items", {"id": 1, "val": "inside"})`,
			offending: `{"event":"row","table":"testdata.other","row":{"id":1}}`,
			wantCause: `table "testdata.other" is not in the declared writes`,
		},
		{
			name:      "undeclared-field-dead-letters-and-commits-nothing",
			writes:    "writes:\n  - table: testdata.items\n    fields: [id]\n",
			inside:    `emit("testdata.items", {"id": 1})`,
			offending: `{"event":"row","table":"testdata.items","row":{"id":1,"val":"sneak"}}`,
			wantCause: `field "val" of table "testdata.items" is not in the declared writes`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ensurePython(t)
			freshDatabases(t)
			bin := Build(t)
			ws := shortWorkspace(t)

			// The script violates exactly once (marker file, so the recycled
			// session stays quiet afterwards): one in-declaration row, then the
			// offending frame verbatim. The engine ends the turn at the violation;
			// later turns answer bare done, so the lane parks after the single
			// dead letter.
			script := "import os\n" + PyTurnPrelude + `
def on_turn(turn, rows):
    if os.path.exists("sent.marker"):
        done(turn)
        return
    open("sent.marker", "w").close()
    ` + tc.inside + `
    sys.stdout.write('` + tc.offending + `' + "\n")
    sys.stdout.flush()
    done(turn)

turn_loop(on_turn)
`
			decl := "name: wviol\nrun: [python, main.py]\n" + tc.writes
			writeResidentWorkspace(t, ws, "wviol", decl, script)
			otherDir := filepath.Join(ws, "schemas", "testdata", "other")
			if err := os.MkdirAll(otherDir, 0o755); err != nil {
				t.Fatalf("mkdir other schema: %v", err)
			}
			if err := os.WriteFile(filepath.Join(otherDir, "table.yaml"), []byte(otherSchemaYAML), 0o644); err != nil {
				t.Fatalf("write other table.yaml: %v", err)
			}

			stop := startEngine(t, bin, ws)
			defer stop()
			bin.Run(t, RunOptions{Args: []string{"declare", "apply", "pipelines/wviol"}, Dir: ws, Timeout: time.Minute}).RequireExit(t, 0)

			ctx := context.Background()
			meta, err := pgx.Connect(ctx, metaDSN(t, ws))
			if err != nil {
				t.Fatalf("connect meta: %v", err)
			}
			defer func() { _ = meta.Close(ctx) }()
			data, err := pgx.Connect(ctx, dataDSN(t, ws))
			if err != nil {
				t.Fatalf("connect data: %v", err)
			}
			defer func() { _ = data.Close(ctx) }()

			// The violating first turn mints its run directly dead-lettered; wait
			// for that first record and read its whole disposition.
			var cause, reason, dlErr string
			var exitNull, found bool
			dl := time.Now().Add(120 * time.Second)
			for time.Now().Before(dl) {
				err := meta.QueryRow(ctx,
					`SELECT r.cause, r.exit_code IS NULL, coalesce(d.reason,''), coalesce(d.error,'')
FROM runs r LEFT JOIN dead_letters d ON d.run_id = r.id
WHERE r.pipeline='wviol' AND r.state='dead_lettered' ORDER BY r.id LIMIT 1`).Scan(&cause, &exitNull, &reason, &dlErr)
				if err == nil {
					found = true
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			if !found {
				t.Fatal("the violating turn produced no dead-lettered run within 120s")
			}
			if cause != "loop" {
				t.Errorf("dead-lettered run cause = %q, want loop (a fresh turn mints directly dead-lettered)", cause)
			}
			if !exitNull {
				t.Error("dead-lettered run carries an exit code; a violated turn records none")
			}
			if reason != "failed" {
				t.Errorf("dead letter reason = %q, want failed", reason)
			}
			if !strings.Contains(dlErr, "turn protocol violation") {
				t.Errorf("dead letter error does not name the protocol violation: %s", dlErr)
			}
			if !strings.Contains(dlErr, tc.wantCause) {
				t.Errorf("dead letter error does not name the violation cause %q: %s", tc.wantCause, dlErr)
			}
			if !strings.Contains(dlErr, strconv.Quote(tc.offending)) {
				t.Errorf("dead letter error does not quote the offending line %s: %s", strconv.Quote(tc.offending), dlErr)
			}

			// Nothing committed: not the offending row, not the in-declaration row
			// framed before it in the same turn, and no journal entry -- the
			// violated turn's data transaction never commits anything.
			for _, probe := range []struct{ what, sql string }{
				{"testdata.items", "SELECT count(*) FROM testdata.items"},
				{"testdata.other", "SELECT count(*) FROM testdata.other"},
				{"public.data_journal", "SELECT count(*) FROM public.data_journal"},
			} {
				var n int
				if err := data.QueryRow(ctx, probe.sql).Scan(&n); err != nil {
					t.Fatalf("count %s: %v", probe.what, err)
				}
				if n != 0 {
					t.Errorf("%s holds %d rows after the violation, want 0 (the turn commits nothing)", probe.what, n)
				}
			}
		})
	}
}
