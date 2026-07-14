package dispatch

// This file is the declaration destroy op: the leader-side path that tears down one
// declared unit. A pipeline destroy reverts the target's un-promoted disposable data,
// then retires all of its meta rows in one atomic transaction (the pipelines row
// last) through the single meta writer, then deletes its object-store bytes. A
// composer destroy clears the lane's rows, but only once the lane has at most one
// registered member -- the mirror of apply's 2+ invariant. This is the dispatch-level
// surface; the CLI and daemon control-connection wiring that drives it is wired at
// the daemon layer.
//
// The teardown's side effects ride four seams, each supplied by the daemon's
// leader wiring (leadership.go) and defaulting to open/no-op only for
// compositions that wire none (shape tests):
//
//   - DataReverter reverts the target's un-promoted disposable data before any
//     meta row is retired: the daemon adapts the journal-driven reverse-replay
//     (pg.Client.ExecuteWipe, scoped to the target with live run attribution).
//   - RunLister feeds the two pre-retirement reads: the remaining runs in
//     archival shape (their run_summaries rows are written inside the retirement
//     transaction, so pruned lineage never dangles) and the artifact-hash census.
//   - ObjectDeleter deletes the target's content-addressed object-store bytes
//     (the artifact FILES; the artifact meta ROWS ride the retirement
//     transaction) after the retirement commits, from the pre-read hash list.
//   - DestroyBlocker is the downstream-blocker predicate set (a registered
//     pipeline depends_on the target, a run_inputs row names its runs, a
//     dead-letter entry names it as failed_upstream). The predicates are pure in
//     destructive.go (DestroyBlockReasons); the daemon feeds them in over live
//     meta snapshots (Candidate.destroyBlocker), and no flag overrides a refusal.

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-lakehouse/internal/store"
)

// LaneComposerDestroyable reports whether a lane composer is destroyable given the
// number of its lane's registered members: destroyable only once the lane has at
// most one registered member, the mirror of apply's 2+ invariant (a pipeline apply
// leaving its lane with 2+ registered members needs a composer; so a composer is
// removable only when the lane no longer needs it to order 2+ members). It is pure,
// so the interlock decision is provable in isolation.
func LaneComposerDestroyable(registeredMembers int) bool {
	return registeredMembers <= 1
}

// DataReverter reverts a pipeline's un-promoted disposable data as part of its
// teardown (destroy reverts un-promoted disposable data along with the
// registration, role, and grants). The destroy flow invokes this seam before the
// meta teardown; the daemon supplies the journal-driven reverse-replay (the same
// ExecuteWipe a scoped workload wipe runs), and the no-op default stands in only
// for compositions that wire none.
type DataReverter interface {
	// RevertUnpromoted reverts the pipeline's un-promoted disposable writes. It runs
	// before any meta row is retired, so a failure leaves meta untouched.
	RevertUnpromoted(ctx context.Context, pipeline string) error
}

// ObjectDeleter deletes a pipeline's object-store bytes (its content-addressed
// artifact files) as part of teardown. The artifact meta rows ride the retirement
// transaction; the files under objects_path cannot, so they are a filesystem side
// effect this seam owns. The daemon supplies a store.ObjectStore-backed deleter;
// the no-op default stands in for compositions that wire none.
type ObjectDeleter interface {
	// DeleteObjects removes the pipeline's object-store bytes: the content
	// hashes, read from the artifacts index BEFORE the retirement deleted its
	// rows, are passed in. It runs after the meta retirement commits, so bytes
	// are freed only once the index rows naming them are gone.
	DeleteObjects(ctx context.Context, pipeline string, hashes []string) error
}

// DestroyBlocker is the downstream-blocker predicate the destroy op consults before
// tearing a pipeline down (destroy refuses while any registered pipeline declares
// depends_on the target, any downstream run_inputs row names its runs, or any
// outstanding dead-letter entry names it as failed_upstream). DestroyBlockReasons
// in destructive.go computes exactly those predicates over snapshots, but nothing
// feeds it in: this seam defaults OPEN, so the teardown action is wired and
// unguarded.
type DestroyBlocker interface {
	// Blocked reports whether the pipeline's teardown is blocked, a human reason when
	// it is, and any error consulting the blocker. The default (openBlocker) never
	// blocks.
	Blocked(ctx context.Context, pipeline string) (blocked bool, reason string, err error)
}

// RunLister lists the prunable runs and artifact hashes for a pipeline so
// destroy can write the runs' archival summaries and free the artifact bytes.
// Both reads run BEFORE the retirement transaction deletes the rows they read.
// The daemon supplies a store.RetentionReader-backed lister; the no-op default
// (no runs, no hashes) stands in for compositions that wire none.
type RunLister interface {
	// ListPrunableRuns returns the pipeline's remaining runs in archival shape,
	// for the summaries written inside the retirement transaction.
	ListPrunableRuns(ctx context.Context, pipeline string) ([]store.PrunableRun, error)
	// ListArtifactHashes returns the pipeline's content-addressed artifact
	// hashes, for the object bytes freed after the retirement commits.
	ListArtifactHashes(ctx context.Context, pipeline string) ([]string, error)
}

// noopRunLister is the default RunLister: no runs and no hashes listed, so no
// summaries are written and no bytes freed unless a real reader is wired.
type noopRunLister struct{}

func (noopRunLister) ListPrunableRuns(context.Context, string) ([]store.PrunableRun, error) {
	return nil, nil
}

func (noopRunLister) ListArtifactHashes(context.Context, string) ([]string, error) {
	return nil, nil
}

// openBlocker is the default DestroyBlocker: it never blocks (destroy proceeds). It
// is the honest default -- the downstream predicates are written
// (DestroyBlockReasons in destructive.go) but no caller supplies them to a
// Destroyer.
type openBlocker struct{}

func (openBlocker) Blocked(context.Context, string) (bool, string, error) { return false, "", nil }

// noopReverter is the default DataReverter: it reverts nothing. It stands in
// only for compositions that wire no reverter; the daemon supplies the
// journal-driven reverse-replay.
type noopReverter struct{}

func (noopReverter) RevertUnpromoted(context.Context, string) error { return nil }

// noopObjectDeleter is the default ObjectDeleter: it deletes no bytes. It stands
// in only for compositions that wire no deleter; the daemon supplies the
// store.ObjectStore-backed one.
type noopObjectDeleter struct{}

func (noopObjectDeleter) DeleteObjects(context.Context, string, []string) error { return nil }

// InterlockError is the composer-destroy interlock refusal: the lane still has two or
// more registered members, so its composer cannot be destroyed (drop members first).
// It names the lane and the count so the CLI surfaces actionable guidance.
type InterlockError struct {
	// Lane is the lane whose composer destroy was refused.
	Lane string
	// Registered is the number of registered members that block the destroy.
	Registered int
}

// Error renders the interlock refusal, naming the lane.
func (e *InterlockError) Error() string {
	return fmt.Sprintf("lane %q still has %d registered members; destroy them before its composer (a composer is removable only once its lane has at most one registered member)", e.Lane, e.Registered)
}

// BlockedError is a destroy refused by the downstream-blocker predicate: a
// dependent, consumer, or dead-letter entry still names the target. It carries the
// blocker's reason so the CLI can tell the operator what to drop or drain first.
type BlockedError struct {
	// Pipeline is the target whose destroy was blocked.
	Pipeline string
	// Reason is the blocker's human explanation (which dependent/consumer/entry).
	Reason string
}

// Error renders the blocked refusal with its reason.
func (e *BlockedError) Error() string {
	return fmt.Sprintf("pipeline %q cannot be destroyed: %s", e.Pipeline, e.Reason)
}

// Destroyer tears down one declared unit through the single meta writer. Build it
// with NewDestroyer over the registry read seam (for the composer interlock's
// registered-member count) and the single-writer submitter (the Dispatcher);
// options supply the DataReverter, ObjectDeleter, and DestroyBlocker seams, each
// defaulting to the honest open/no-op value.
type Destroyer struct {
	reg       store.RegistryReader
	submit    Submitter
	reverter  DataReverter
	objects   ObjectDeleter
	blocker   DestroyBlocker
	runLister RunLister
}

// DestroyerOption configures a Destroyer's seams at construction.
type DestroyerOption func(*Destroyer)

// WithDataReverter sets the un-promoted-data revert seam (the journal-driven
// reverse-replay). A nil reverter is ignored, keeping the no-op default.
func WithDataReverter(r DataReverter) DestroyerOption {
	return func(d *Destroyer) {
		if r != nil {
			d.reverter = r
		}
	}
}

// WithObjectDeleter sets the object-store-bytes deletion seam (the
// content-addressed deletion of the target's artifact files). A nil deleter is
// ignored, keeping the no-op default.
func WithObjectDeleter(o ObjectDeleter) DestroyerOption {
	return func(d *Destroyer) {
		if o != nil {
			d.objects = o
		}
	}
}

// WithDestroyBlocker sets the downstream-blocker predicate seam (say
// DestroyBlockReasons adapted through DestroyBlockerFunc). A nil blocker is ignored,
// keeping the open default (never blocks).
func WithDestroyBlocker(b DestroyBlocker) DestroyerOption {
	return func(d *Destroyer) {
		if b != nil {
			d.blocker = b
		}
	}
}

// WithRunLister sets the run lister used to archive remaining runs' summaries
// during destroy. A nil lister is ignored.
func WithRunLister(l RunLister) DestroyerOption {
	return func(d *Destroyer) {
		if l != nil {
			d.runLister = l
		}
	}
}

// NewDestroyer builds the destroy op over the registry reader and the single-writer
// submitter, with the seams defaulting to open/no-op.
func NewDestroyer(reg store.RegistryReader, submit Submitter, opts ...DestroyerOption) *Destroyer {
	d := &Destroyer{
		reg:       reg,
		submit:    submit,
		reverter:  noopReverter{},
		objects:   noopObjectDeleter{},
		blocker:   openBlocker{},
		runLister: noopRunLister{},
	}
	for _, o := range opts {
		o(d)
	}
	return d
}

// DestroyPipeline tears down one pipeline: it consults the downstream blocker, reverts
// the target's un-promoted disposable data, retires all of the pipeline's meta rows in
// one atomic transaction (pipelines row last) through the single writer, then deletes
// its object-store bytes. The steps are ordered so a failure never leaves a
// half-torn unit: a block or a revert failure returns before any meta write, and the
// retirement is all-or-nothing. The object-bytes deletion runs only after the meta
// index rows naming them are gone.
func (d *Destroyer) DestroyPipeline(ctx context.Context, name string) error {
	blocked, reason, err := d.blocker.Blocked(ctx, name)
	if err != nil {
		return fmt.Errorf("dispatch: destroy pipeline %q: check blockers: %w", name, err)
	}
	if blocked {
		return &BlockedError{Pipeline: name, Reason: reason}
	}

	// Revert the un-promoted disposable data before any meta row is retired, so a
	// revert failure leaves meta exactly as it was (the default reverter is a no-op).
	if err := d.reverter.RevertUnpromoted(ctx, name); err != nil {
		return fmt.Errorf("dispatch: destroy pipeline %q: revert un-promoted data: %w", name, err)
	}

	// Archive summaries for remaining runs before the retirement deletes, inside
	// the same transaction so stamps keep resolving. A failed read refuses the
	// destroy: retiring runs without their summaries would leave journal stamps
	// resolving to nothing.
	runs, err := d.runLister.ListPrunableRuns(ctx, name)
	if err != nil {
		return fmt.Errorf("dispatch: destroy pipeline %q: list remaining runs: %w", name, err)
	}
	var sums []store.RunSummary
	for _, r := range runs {
		sums = append(sums, store.BuildRunSummary(r))
	}

	// Read the artifact hashes BEFORE the retirement deletes the index rows that
	// name them: the bytes are freed after the commit, but only this read knows
	// which files are the target's.
	hashes, err := d.runLister.ListArtifactHashes(ctx, name)
	if err != nil {
		return fmt.Errorf("dispatch: destroy pipeline %q: list artifact hashes: %w", name, err)
	}

	// Retire the pipeline's rows in one atomic meta transaction, pipelines row last.
	if err := d.submit.Submit(ctx, func(w *store.Writer) error {
		if len(sums) > 0 {
			return w.RetirePipelineWithSummaries(ctx, name, sums)
		}
		return w.RetirePipeline(ctx, name)
	}); err != nil {
		return fmt.Errorf("dispatch: destroy pipeline %q: %w", name, err)
	}

	// Free the object-store bytes only after the meta index rows are gone (the default
	// deleter is a no-op, so nothing is freed unless one is supplied).
	if err := d.objects.DeleteObjects(ctx, name, hashes); err != nil {
		return fmt.Errorf("dispatch: destroy pipeline %q: delete object bytes: %w", name, err)
	}
	return nil
}

// DestroyComposer tears down one lane composer: it clears the lane's rows through the
// single writer, but only once the lane has at most one registered member (the mirror
// of apply's 2+ invariant). members are the lane's declared member names; the count
// that are registered decides the interlock, so a lane still ordering 2+ registered
// members refuses the destroy (InterlockError, naming the lane) and writes nothing.
func (d *Destroyer) DestroyComposer(ctx context.Context, lane string, members []string) error {
	registered, err := d.countRegistered(ctx, members)
	if err != nil {
		return fmt.Errorf("dispatch: destroy composer %q: %w", lane, err)
	}
	if !LaneComposerDestroyable(registered) {
		return &InterlockError{Lane: lane, Registered: registered}
	}
	// Clear the lane's rows: an empty order rewrite deletes every member row, the same
	// atomic full-lane rewrite an apply uses, so the lane returns to nominal.
	if err := d.submit.Submit(ctx, func(w *store.Writer) error {
		return w.RewriteLane(ctx, lane, nil)
	}); err != nil {
		return fmt.Errorf("dispatch: destroy composer %q: %w", lane, err)
	}
	return nil
}

// countRegistered returns how many of members are registered pipelines, read from the
// current registry. It is the composer interlock's registered-member count.
func (d *Destroyer) countRegistered(ctx context.Context, members []string) (int, error) {
	names, err := d.reg.RegisteredPipelines(ctx)
	if err != nil {
		return 0, fmt.Errorf("read registry: %w", err)
	}
	isRegistered := make(map[string]bool, len(names))
	for _, n := range names {
		isRegistered[n] = true
	}
	seen := make(map[string]bool, len(members))
	count := 0
	for _, m := range members {
		if seen[m] {
			continue
		}
		seen[m] = true
		if isRegistered[m] {
			count++
		}
	}
	return count, nil
}
