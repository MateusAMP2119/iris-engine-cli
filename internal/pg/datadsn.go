package pg

import (
	"fmt"

	"github.com/jackc/pgx/v5"
)

// DataDSN retargets an admin/maintenance DSN to the engine-owned data database,
// returning a URL-form DSN a run's scoped connection (and a run's own psql) can use
// directly. The admin DSN points at a connectable maintenance database (the cluster
// default, e.g. postgres); the declared tables, the capture trigger, and the data
// journal all live in the dedicated data database (DataDatabase), which pg.Connect
// points the daemon's own pool at (live.go). A lane or manual run's scoped connection
// must target that same database so its writes land where the capture trigger attributes
// them, so the daemon derives the run's IRIS_DB_URL from this, then appends the run-scoped
// iris.run_id option with InjectRunID.
//
// The DSN is rebuilt in URL form (sslmode=disable) from the parsed admin config, the same
// shape the conformance harness derives the data DSN in, so InjectRunID's query-parameter
// append composes cleanly on top.
func DataDSN(adminDSN string) (string, error) {
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		return "", fmt.Errorf("pg: parse admin DSN for data target: %w", err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, DataDatabase), nil
}
