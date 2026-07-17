package pg

import (
	"context"
	"fmt"
)

// This file is the data-database half of the ps load history: the engine-owned
// table the daemon's load collector seals its coarse (per-minute maximum) load
// buckets into, reads back at start to re-seed its in-memory rings, and prunes
// on a rolling window. It lives in the iris schema beside turn_positions,
// where no pipeline role is granted table access -- engine bookkeeping, not
// user data. Rows are keyed by node (the sampling host: ps(1) load is a
// per-host truth), series (the collector's entity key: "engine",
// "lane:<name>", "pipeline:<name>"), and the bucket's seal time in unix
// seconds. A bucket that saw no sample persists NULL load -- absence over
// fabrication, in the schema itself. The wire never carries these timestamps;
// they exist so the seeding daemon can size the downtime gap as absent slots.

// LoadHistoryName is the engine-owned load-history table.
const LoadHistoryName = CaptureSchema + ".load_history"

// loadHistoryDDL is the create-if-missing DDL for the load-history table.
const loadHistoryDDL = `CREATE TABLE IF NOT EXISTS ` + LoadHistoryName + ` (
    node text NOT NULL,
    series text NOT NULL,
    bucket bigint NOT NULL,
    cpu_max double precision,
    rss_max bigint,
    PRIMARY KEY (node, series, bucket)
);`

// loadHistoryBucketIndexDDL indexes the bucket column alone, the prune's scan.
const loadHistoryBucketIndexDDL = `CREATE INDEX IF NOT EXISTS load_history_bucket ON ` + LoadHistoryName + ` (bucket);`

// EnsureLoadHistory ensures the iris schema and the engine-owned load-history
// table exist. It is idempotent (create-if-missing) and self-contained, so the
// daemon can call it at every start; a dropped table self-heals at the next
// call.
func EnsureLoadHistory(ctx context.Context, db DB) error {
	for _, stmt := range []string{CaptureSchemaDDL(), loadHistoryDDL, loadHistoryBucketIndexDDL} {
		if err := db.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("pg: ensure load history: %w", err)
		}
	}
	return nil
}

// LoadBucket is one sealed coarse load bucket: the series it belongs to, its
// seal time in unix seconds, and the bucket's maximum CPU and resident-memory
// samples. Sampled false means the bucket saw no sample at all; it persists
// (and reads back) as NULL load.
type LoadBucket struct {
	// Series is the collector's entity key: "engine", "lane:<name>", or
	// "pipeline:<name>".
	Series string
	// Bucket is the bucket's seal time in unix seconds.
	Bucket int64
	// CPUMax is the bucket's maximum sampled CPU percent (0 when unsampled).
	CPUMax float64
	// RSSMax is the bucket's maximum sampled resident memory in bytes.
	RSSMax int64
	// Sampled reports whether the bucket saw any sample.
	Sampled bool
}

// WriteLoadBuckets persists one seal's buckets for node in a single
// transaction. Conflicting rows (a crash-replayed seal, two same-host daemons
// racing a bucket) are left untouched: the first write wins, and a re-write
// never rewrites history.
func (c *Client) WriteLoadBuckets(ctx context.Context, node string, buckets []LoadBucket) error {
	if len(buckets) == 0 {
		return nil
	}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: begin load-history write: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // safe no-op after commit
	for _, b := range buckets {
		var cpu *float64
		var rss *int64
		if b.Sampled {
			cpu, rss = &b.CPUMax, &b.RSSMax
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+LoadHistoryName+` (node, series, bucket, cpu_max, rss_max)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT (node, series, bucket) DO NOTHING`,
			node, b.Series, b.Bucket, cpu, rss); err != nil {
			return fmt.Errorf("pg: write load bucket %s/%s: %w", node, b.Series, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: commit load-history write: %w", err)
	}
	return nil
}

// ReadLoadHistory reads node's persisted buckets sealed at or after since
// (unix seconds), in bucket order -- the seeding read the daemon replays into
// its coarse rings at start.
func (c *Client) ReadLoadHistory(ctx context.Context, node string, since int64) ([]LoadBucket, error) {
	rows, err := c.pool.Query(ctx, `SELECT series, bucket, cpu_max, rss_max FROM `+LoadHistoryName+`
WHERE node = $1 AND bucket >= $2 ORDER BY bucket, series`, node, since)
	if err != nil {
		return nil, fmt.Errorf("pg: read load history: %w", err)
	}
	defer rows.Close()
	var out []LoadBucket
	for rows.Next() {
		var b LoadBucket
		var cpu *float64
		var rss *int64
		if err := rows.Scan(&b.Series, &b.Bucket, &cpu, &rss); err != nil {
			return nil, fmt.Errorf("pg: scan load bucket: %w", err)
		}
		if cpu != nil {
			b.CPUMax, b.Sampled = *cpu, true
		}
		if rss != nil {
			b.RSSMax = *rss
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: iterate load history: %w", err)
	}
	return out, nil
}

// PruneLoadHistory deletes every node's buckets sealed before the cutoff (unix
// seconds) -- the rolling retention window. Pruning all nodes keeps rows of
// decommissioned hosts from lingering forever.
func (c *Client) PruneLoadHistory(ctx context.Context, before int64) error {
	if _, err := c.pool.Exec(ctx, `DELETE FROM `+LoadHistoryName+` WHERE bucket < $1`, before); err != nil {
		return fmt.Errorf("pg: prune load history: %w", err)
	}
	return nil
}
