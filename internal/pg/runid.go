package pg

import (
	"strconv"
	"strings"
)

// This file owns the mechanism by which a run's id rides the data connection Iris
// injects into it at spawn (specification section 4: "run id rides the injected
// connection (per-session setting at spawn), trigger-read in-transaction; no row keyed
// to a role without a run"). The capture trigger (capture.go) reads the per-session
// setting current_setting('iris.run_id') in-transaction to attribute every captured
// write to its run; this is the write side of that read. It sets the setting on the
// connection itself -- not through the run's own (arbitrary, author-supplied) code --
// so attribution cannot be forgotten or forged: the setting rides the DSN's libpq
// `options` connection parameter, applied once at connection startup, covering every
// statement the run issues and unremovable by the pipeline.

// RunIDSetting is the name of the per-session Postgres configuration setting (a custom,
// dotted-namespace GUC) that carries a run's id on its injected data connection. It
// mirrors the setting the capture trigger reads with current_setting (capture.go); the
// two must name the same GUC for a write to be attributed. Engine-owned and stable.
const RunIDSetting = "iris.run_id"

// RunIDConnOptions renders the libpq `options` connection-parameter VALUE that sets the
// per-session run id at connection startup: "-c iris.run_id=<runID>". A client applies
// it as a startup command-line option, so the GUC is set for the whole session before
// the run issues its first statement. runID is the run's meta identity (runs.id, a
// bigint), so the value is always a well-formed integer literal and no option-injection
// is possible. It is the value InjectRunID places under a DSN's `options` query
// parameter.
func RunIDConnOptions(runID int64) string {
	return "-c " + RunIDSetting + "=" + strconv.FormatInt(runID, 10)
}

// InjectRunID returns dsn carrying the run's id as the per-session iris.run_id GUC the
// capture trigger reads in-transaction, by appending RunIDConnOptions(runID) under the
// DSN's libpq `options` query parameter. The rest of the DSN -- scheme, credentials,
// host, database, and any existing query parameters -- is preserved byte-for-byte as a
// prefix, so the returned DSN differs from dsn only by the appended option. The engine
// calls this at spawn on the scoped connection it injects as IRIS_DB_URL, so the run's
// writes are attributed to runID by the capture trigger without the run's own code
// setting anything.
//
// The single character in the option value needing URI encoding is the space that
// separates -c from the setting; it is encoded as %20, which every conforming
// connection-URI client (libpq and pgx alike) decodes back to a space. ('+' is decoded
// to a space only under form-encoding, which a connection URI does not use, so %20 is
// the portable choice.) The engine's scoped connection carries no pre-existing
// `options` parameter, so a single fresh `options` is always appended.
func InjectRunID(dsn string, runID int64) string {
	value := strings.ReplaceAll(RunIDConnOptions(runID), " ", "%20")
	sep := "?"
	if strings.ContainsRune(dsn, '?') {
		sep = "&"
	}
	return dsn + sep + "options=" + value
}
