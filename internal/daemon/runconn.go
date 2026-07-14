package daemon

import (
	"context"
	"io"
	"log/slog"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// This file builds the data connection a run is handed in IRIS_DB_URL. A run
// connects as its OWN least-privilege pipeline login role -- provisioned at
// declare apply with exactly the declaration's field grants, its credential
// persisted create-once in meta -- never as the engine's admin identity. The
// run id rides the DSN as the iris.run_id GUC either way, so capture
// attribution is identical on both paths. A pipeline with no persisted
// credential (applied before role provisioning existed, or a credential read
// failure) falls back to the admin-derived data DSN with a warning, so an
// upgrade never strands a working pipeline -- the next `iris declare apply`
// provisions its role and the fallback disappears.

// runConnBuilder derives each run's scoped IRIS_DB_URL.
type runConnBuilder struct {
	// params are the data-database connection parameters (host, port, database,
	// options) the scoped DSN re-targets with the pipeline role's identity.
	params store.ScopedConnParams
	// creds reads a role's persisted credential from meta.
	creds store.RoleCredentialReader
	// fallback is the admin-derived data DSN used when the pipeline has no
	// persisted credential.
	fallback string
	logger   *slog.Logger
}

// newRunConnBuilder builds the run-connection builder over the admin-derived
// data DSN (whose host/port/database/options the scoped connections share) and
// the credential read seam. A nil creds reader leaves every run on the fallback
// DSN (shape-test compositions).
func newRunConnBuilder(dataDSN string, creds store.RoleCredentialReader, logger *slog.Logger) (*runConnBuilder, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	params, err := store.ScopedParamsFromDSN(dataDSN)
	if err != nil {
		return nil, err
	}
	return &runConnBuilder{params: params, creds: creds, fallback: dataDSN, logger: logger}, nil
}

// dsnFor returns the run's IRIS_DB_URL: the pipeline role's scoped connection
// carrying the run id GUC, or the admin-derived fallback (warned) when the role
// has no persisted credential.
func (b *runConnBuilder) dsnFor(ctx context.Context, pipeline string, runID int64) string {
	if b == nil {
		return "" // no data connection wired at all (shape tests)
	}
	if b.creds != nil {
		role := pg.PipelineRoleName(pipeline)
		secret, ok, err := b.creds.RoleSecret(ctx, role)
		switch {
		case err != nil:
			b.logger.Warn("run conn: read pipeline role credential; falling back to the admin-derived DSN", "pipeline", pipeline, "err", err)
		case !ok:
			b.logger.Warn("run conn: pipeline role has no persisted credential; falling back to the admin-derived DSN (re-run `iris declare apply` to provision the role)", "pipeline", pipeline, "role", role)
		default:
			conn, cerr := store.BuildScopedConn(b.params, role, secret)
			if cerr != nil {
				b.logger.Warn("run conn: build scoped connection; falling back to the admin-derived DSN", "pipeline", pipeline, "err", cerr)
				break
			}
			return pg.InjectRunID(conn.EnvValue(), runID)
		}
	}
	if b.fallback == "" {
		return ""
	}
	return pg.InjectRunID(b.fallback, runID)
}
