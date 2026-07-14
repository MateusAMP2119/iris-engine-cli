package daemon

import (
	"context"

	"github.com/MateusAMP2119/iris-lakehouse/internal/pg"
)

// This file is the leader-side grant-drift reconciliation: on winning
// leadership, every ledgered data-PAT role's live Postgres grants are diffed
// against the meta access ledger (pg.Reconcile over the live catalog read). The
// ledger is authoritative -- a grant Postgres lost out-of-band is re-issued --
// and a STRAY, a grant Postgres holds beyond the ledger (someone widened a
// token's read access directly on the cluster), is reported in the daemon log,
// never silently revoked. Without this pass a data PAT's grants were asserted
// exactly once, at mint, and never re-consulted.

// reconcileGrantDrift runs one drift pass over every ledgered data-PAT role. It
// is best-effort: any failure is logged and leadership proceeds -- drift
// detection is a reporting duty, never a dispatch gate.
func (c *Candidate) reconcileGrantDrift(ctx context.Context) {
	if c.patGrantLedger == nil || c.data == nil {
		return // drift detection not wired (shape-test compositions)
	}
	ledgers, err := c.patGrantLedger.DataPATRoleGrants(ctx)
	if err != nil {
		c.logger.Warn("grant drift: read data-PAT grant ledger", "err", err)
		return
	}
	for _, rl := range ledgers {
		plan, err := pg.Reconcile(ctx, c.data, c.data, rl.Role, rl.Grants)
		if err != nil {
			c.logger.Warn("grant drift: reconcile role", "role", rl.Role, "err", err)
			continue
		}
		if len(plan.Grants) > 0 {
			c.logger.Warn("grant drift: re-issued ledgered grants Postgres had lost",
				"role", rl.Role, "granted", len(plan.Grants))
		}
		for _, stray := range plan.Strays {
			c.logger.Warn("grant drift: STRAY grant beyond the ledger (widened out-of-band; never revoked automatically)",
				"role", rl.Role, "detail", stray.Detail)
		}
	}
	if len(ledgers) > 0 {
		c.logger.Info("grant drift: reconciled data-PAT roles against the ledger", "roles", len(ledgers))
	}
}
