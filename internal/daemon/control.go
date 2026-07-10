package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/MateusAMP2119/iris-engine-cli/internal/api"
	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/dispatch"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
)

// This file is the daemon's leader-side control plane: the composition root that
// turns POST /apply and POST /destroy into the registry apply, schema provisioning,
// and scoped teardown (specification sections 3, 6.3, and 12). It sits at the top of
// the import graph (daemon composes dispatch, pg, store, and declare) and is the one
// place they are wired together, so no lower package reaches across the meta/data
// boundary.
//
// The control plane is leader-only. The api mux gates every mutation to the leader
// (a standby returns not_leader), and the dispatcher -- the single meta writer -- only
// exists once a candidate wins the lock. So the live orchestrator is installed on
// winning leadership (before the leader role is reported) and cleared on demotion; a
// swappable controlPlane holds it and satisfies api.ControlHandler for the whole
// daemon lifetime, so the mux binds to a stable handler.
//
// Workspace resolution (the simplification the task adopts, aligned with the E11
// candidate-requires-workspace rule): the CLI reads local files only to validate,
// then sends the workspace-relative path; the leader resolves the declaration and the
// schemas/ tree against its own workspace tree. In the single-host case they are the
// same tree.

// dataPlane is the data-database surface the control plane provisions through: the DDL
// exec seam, the live-view reader the provisioner diffs against, and the
// capture-function forward seam that lets the capture triggers bind. *pg.Client
// satisfies it; a fake can stand in for tests.
type dataPlane interface {
	pg.DB
	pg.LiveViewReader
	// EnsureCaptureFunction ensures iris.capture() exists so provisioning's capture
	// triggers bind (the E03.10 forward seam; E06.2 owns the real body).
	EnsureCaptureFunction(ctx context.Context) error
	// ExecuteWipe runs a workload wipe (or destroy revert) on the data database.
	// Added here so the same client instance wires to wipe plane without extra
	// seam (wipe and provision share the data client).
	ExecuteWipe(ctx context.Context, target pg.WipeTarget) (pg.WipeResult, error)
}

// controlPlane is the daemon's api.ControlHandler: a stable handle the mux binds to
// for the daemon's whole life, delegating to the live orchestrator when the daemon
// leads and returning an internal fault otherwise. Leadership installs the
// orchestrator before reporting the leader role and clears it on demotion, so a
// mutation only ever reaches an installed orchestrator (the mux gates to leader too).
type controlPlane struct {
	mu   sync.RWMutex
	live *controlOrchestrator
}

// compile-time proof the control plane is the mux's control handler.
var _ api.ControlHandler = (*controlPlane)(nil)

// newControlPlane returns an unwired control plane: mutations fault until a leader
// installs an orchestrator.
func newControlPlane() *controlPlane { return &controlPlane{} }

// install wires the live orchestrator (on winning leadership).
func (c *controlPlane) install(o *controlOrchestrator) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = o
}

// clear removes the orchestrator (on demotion), so a lingering request after a lost
// lock faults rather than mutating meta off the single-writer path.
func (c *controlPlane) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.live = nil
}

// orchestrator returns the installed orchestrator, or nil when the daemon is not
// leading.
func (c *controlPlane) orchestrator() *controlOrchestrator {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.live
}

// Apply routes to the live orchestrator, or faults when none is installed.
func (c *controlPlane) Apply(ctx context.Context, req api.ControlRequest) (api.ControlResult, error) {
	o := c.orchestrator()
	if o == nil {
		return api.ControlResult{}, api.ErrControlUnavailable
	}
	return o.apply(ctx, req)
}

// Destroy routes to the live orchestrator, or faults when none is installed.
func (c *controlPlane) Destroy(ctx context.Context, req api.ControlRequest) (api.ControlResult, error) {
	o := c.orchestrator()
	if o == nil {
		return api.ControlResult{}, api.ErrControlUnavailable
	}
	return o.destroy(ctx, req)
}

// controlOrchestrator runs the leader-side control mutations against the workspace and
// the databases. It composes the registry applier, the scoped destroyer, the schema
// provisioner (over the data plane and the applied-head reader), and the ledger
// recorder (the single-writer meta write for applied heads).
type controlOrchestrator struct {
	workspace string
	applier   *dispatch.Applier
	destroyer *dispatch.Destroyer
	registry  store.RegistryReader
	data      dataPlane
	ledgerRec pg.LedgerRecorder
	heads     store.AppliedHeadReader
	logger    *slog.Logger
}

// newControlOrchestrator builds the leader's control orchestrator over its workspace
// root and the wired seams. reg is the plain-MVCC registry reader the composer-destroy
// interlock counts a lane's registered members from (the lanes table in meta, not the
// workspace disk). A nil logger discards output.
func newControlOrchestrator(workspace string, applier *dispatch.Applier, destroyer *dispatch.Destroyer, reg store.RegistryReader, data dataPlane, ledgerRec pg.LedgerRecorder, heads store.AppliedHeadReader, logger *slog.Logger) *controlOrchestrator {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &controlOrchestrator{
		workspace: workspace,
		applier:   applier,
		destroyer: destroyer,
		registry:  reg,
		data:      data,
		ledgerRec: ledgerRec,
		heads:     heads,
		logger:    logger,
	}
}

// resolveTarget resolves a request path against the leader's workspace tree: an
// absolute path is taken as-is, a relative one is joined under the workspace, so the
// leader resolves against its own tree regardless of the caller's directory.
func (o *controlOrchestrator) resolveTarget(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(o.workspace, path)
}

// apply registers and provisions the one declaration named by req, idempotently: it
// resolves the target against the workspace, persists the pipeline or composer through
// the single meta writer (the registry apply), and rides schema provisioning on the
// apply. A dry run resolves and previews but writes nothing.
func (o *controlOrchestrator) apply(ctx context.Context, req api.ControlRequest) (api.ControlResult, error) {
	target := o.resolveTarget(req.Path)
	resolved, decl, err := declare.LoadDeclarationFile(target)
	if err != nil {
		return api.ControlResult{}, err
	}

	switch decl.Kind {
	case declare.KindPipeline:
		if !req.DryRun {
			folder, ferr := o.relFolder(resolved)
			if ferr != nil {
				return api.ControlResult{}, ferr
			}
			if err := o.applier.ApplyPipeline(ctx, folder, decl.Pipeline); err != nil {
				return api.ControlResult{}, err
			}
		}
		if err := o.provision(ctx, req.DryRun); err != nil {
			return api.ControlResult{}, err
		}
		return api.ControlResult{Kind: decl.Kind.String(), Target: decl.Pipeline.Name, DryRun: req.DryRun}, nil
	case declare.KindComposer:
		if !req.DryRun {
			if err := o.applier.ApplyComposer(ctx, decl.Composer); err != nil {
				return api.ControlResult{}, err
			}
		}
		if err := o.provision(ctx, req.DryRun); err != nil {
			return api.ControlResult{}, err
		}
		return api.ControlResult{Kind: decl.Kind.String(), Target: decl.Composer.Lane, DryRun: req.DryRun}, nil
	default:
		return api.ControlResult{}, fmt.Errorf("declare apply: unknown declaration kind %v", decl.Kind)
	}
}

// destroy tears down the one declaration named by req: a pipeline destroy retires the
// pipeline (reverting its un-promoted data first), a composer destroy clears the lane
// once its registered-member interlock permits. A dry run resolves but writes nothing.
func (o *controlOrchestrator) destroy(ctx context.Context, req api.ControlRequest) (api.ControlResult, error) {
	target := o.resolveTarget(req.Path)
	_, decl, err := declare.LoadDeclarationFile(target)
	if err != nil {
		return api.ControlResult{}, err
	}

	switch decl.Kind {
	case declare.KindPipeline:
		if !req.DryRun {
			if err := o.destroyer.DestroyPipeline(ctx, decl.Pipeline.Name); err != nil {
				return api.ControlResult{}, err
			}
		}
		return api.ControlResult{Kind: decl.Kind.String(), Target: decl.Pipeline.Name, DryRun: req.DryRun}, nil
	case declare.KindComposer:
		if !req.DryRun {
			members, err := o.laneMembers(ctx, decl.Composer.Lane)
			if err != nil {
				return api.ControlResult{}, err
			}
			if err := o.destroyer.DestroyComposer(ctx, decl.Composer.Lane, members); err != nil {
				return api.ControlResult{}, err
			}
		}
		return api.ControlResult{Kind: decl.Kind.String(), Target: decl.Composer.Lane, DryRun: req.DryRun}, nil
	default:
		return api.ControlResult{}, fmt.Errorf("declare destroy: unknown declaration kind %v", decl.Kind)
	}
}

// laneMembers returns the lane's member names from the lanes table in meta (the
// composer-destroy interlock's registered-member basis), not the workspace disk: a
// pipeline still registered in the lane whose iris-declare.yaml was deleted from disk
// must still be counted, so a composer is never destroyed while registered members
// remain. A read failure refuses the destroy (returns the error) rather than
// proceeding on an unknown member count: the conservative direction, since the
// interlock exists to keep a composer that 2+ registered members still need from
// being removed.
func (o *controlOrchestrator) laneMembers(ctx context.Context, lane string) ([]string, error) {
	members, err := o.registry.LaneMembers(ctx, lane)
	if err != nil {
		return nil, fmt.Errorf("declare destroy: read lane %q members: %w", lane, err)
	}
	return members, nil
}

// provision runs pipeline-independent schema provisioning over the workspace schemas/
// tree (specification section 5): it rejects a reserved public schema folder,
// discovers the declared tables, reconstructs each table's ledger (on-disk migrations
// plus the applied head in meta), reads the live-Postgres view, and plans. On every
// non-dry-run apply it ensures the capture function (self-healing, even when the plan
// is empty) and then applies the plan when non-empty. A re-apply against an
// already-provisioned database plans empty, so provisioning is idempotent (nothing
// re-created, nothing re-recorded) beyond the idempotent capture-function ensure.
func (o *controlOrchestrator) provision(ctx context.Context, dryRun bool) error {
	schemasDir := filepath.Join(o.workspace, "schemas")
	if _, err := os.Stat(schemasDir); errors.Is(err, fs.ErrNotExist) {
		return nil // no schemas/ tree: nothing to provision.
	} else if err != nil {
		return fmt.Errorf("declare apply: stat schemas tree: %w", err)
	}
	// public is engine-reserved: reject a schemas/public/ folder before any
	// provisioning, independent of ValidateSchemaTree's per-table folder-agreement
	// checks, so declared tables never land in the engine's own public schema beside
	// data_journal (specification section 3).
	if err := declare.ValidateSchemaTreeReserved(schemasDir); err != nil {
		return fmt.Errorf("declare apply: %w", err)
	}
	// Provisioning reads only the schemas/ tree (pipeline-independent, specification
	// section 5): it never validates the pipeline folders, so a schema apply provisions
	// even while another pipeline in the workspace is mid-edit.
	schemas, err := declare.ValidateSchemaTree(schemasDir)
	if err != nil {
		return fmt.Errorf("declare apply: read schemas: %w", err)
	}
	if len(schemas) == 0 {
		return nil // no declared tables: nothing to provision.
	}

	heads, err := o.heads.AppliedHeads(ctx)
	if err != nil {
		return fmt.Errorf("declare apply: read applied migration heads: %w", err)
	}
	ledgers := make(map[string]pg.TableLedger, len(schemas))
	for _, dt := range schemas {
		disk, err := pg.LoadDiskMigrations(filepath.Join(dt.Dir, "migrations"))
		if err != nil {
			return fmt.Errorf("declare apply: load migrations for %s.%s: %w", dt.Schema, dt.Table, err)
		}
		key := dt.Schema + "." + dt.Table
		ledgers[key] = pg.TableLedger{DiskMigrations: disk, AppliedHeadID: heads[key]}
	}

	live, err := o.data.ReadLiveView(ctx)
	if err != nil {
		return fmt.Errorf("declare apply: read live view: %w", err)
	}
	plan, err := pg.PlanProvision(schemas, live, ledgers)
	if err != nil {
		return fmt.Errorf("declare apply: plan provision: %w", err)
	}
	if dryRun {
		return nil
	}

	// Ensure iris.capture() on every non-dry-run apply, before the empty-plan early
	// return: the capture triggers bind to it, so a dropped function must be
	// re-created (the seam is self-healing) even when the plan is otherwise empty.
	// Skipping it on an empty plan would leave a re-apply computing nothing and the
	// triggers silently broken (E03.10 forward seam, E06.2 owns the body).
	if err := o.data.EnsureCaptureFunction(ctx); err != nil {
		return fmt.Errorf("declare apply: ensure capture function: %w", err)
	}
	if plan.Empty() {
		return nil
	}
	if err := plan.Apply(ctx, o.data, o.ledgerRec); err != nil {
		return fmt.Errorf("declare apply: provision: %w", err)
	}
	return nil
}

// relFolder returns the workspace-relative folder of a resolved declaration file: the
// pipelines.folder value persisted for a pipeline. It is deterministic given the same
// target, so a re-apply writes the identical folder and stays a no-op.
func (o *controlOrchestrator) relFolder(resolved string) (string, error) {
	rel, err := filepath.Rel(o.workspace, filepath.Dir(resolved))
	if err != nil {
		return "", fmt.Errorf("declare apply: resolve pipeline folder: %w", err)
	}
	return rel, nil
}
