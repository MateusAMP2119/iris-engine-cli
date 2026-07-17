package pg

import (
	"strconv"
	"strings"
)

// Run-attribution GUC: the capture trigger reads it in-transaction; CommitTurn sets it with SET LOCAL (#206); InjectRunID is the connection-level form the conformance harness uses.

// RunIDSetting is the per-session GUC carrying a run's id; must match capture.go's current_setting read.
const RunIDSetting = "iris.run_id"

// RunIDConnOptions renders the libpq `options` value ("-c iris.run_id=<id>") that sets the GUC at connection startup.
func RunIDConnOptions(runID int64) string {
	return "-c " + RunIDSetting + "=" + strconv.FormatInt(runID, 10)
}

// InjectRunID appends the iris.run_id GUC to dsn under libpq `options` (space as %20, '=' as %3D so psql's URI parser accepts it); the harness's attributed-connection helper.
func InjectRunID(dsn string, runID int64) string {
	value := strings.NewReplacer(" ", "%20", "=", "%3D").Replace(RunIDConnOptions(runID))
	sep := "?"
	if strings.ContainsRune(dsn, '?') {
		sep = "&"
	}
	return dsn + sep + "options=" + value
}
