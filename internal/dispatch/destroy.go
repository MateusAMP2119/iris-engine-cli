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
// Three seams are deliberately open here, each documented rather than silently
// stubbed, because the behavior they gate belongs to a later epic:
//
//   - DataReverter reverts the target's un-promoted disposable data. The real
//     journal-driven reverse-replay is E06; today a no-op seam (or a test recorder)
//     stands in, invoked as a recorded step before the meta teardown so the flow and
//     its ordering are proven now and E06 fills only the body.
//   - ObjectDeleter deletes the target's content-addressed object-store bytes (the
//     artifact FILES; the artifact meta ROWS ride the retirement transaction). The
//     real content-addressed deletion is E05/E07; a no-op seam stands in now.
//   - DestroyBlocker is the downstream-blocker predicate set (a registered pipeline
//     depends_on the target, a run_inputs row names its runs, a dead-letter entry
//     names it as failed_upstream). Those predicates are E10.1's; the seam here
//     defaults OPEN (never blocks), so E10.1 supplies the guard without touching this
//     teardown action.
//
// The archival-summary write (each remaining run's run_summaries row, written in the
// retirement transaction so pruned lineage never dangles) is likewise deferred to
// E05/E07's archival tier; store.Writer.RetirePipeline documents that omission.

import (
	"context"
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/store"
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
// teardown (destroy reverts un-promoted disposable data along with the registration,
// role, and grants). The real journal-driven reverse-replay is E06; the destroy flow
// invokes this seam before the meta teardown, so a nil-safe no-op stands in today and
// E06 fills the reverse-replay body.
type DataReverter interface {
	// RevertUnpromoted reverts the pipeline's un-promoted disposable writes. It runs
	// before any meta row is retired, so a failure leaves meta untouched.
	RevertUnpromoted(ctx context.Context, pipeline string) error
}

// ObjectDeleter deletes a pipeline's object-store bytes (its content-addressed
// artifact files) as part of teardown. The artifact meta rows ride the retirement
// transaction; the files under objects_path cannot, so they are a filesystem side
// effect this seam owns. The real deletion is E05/E07; a no-op stands in today.
type ObjectDeleter interface {
	// DeleteObjects removes the pipeline's object-store bytes. It runs after the meta
	// retirement commits, so bytes are freed only once the index rows naming them are
	// gone.
	DeleteObjects(ctx context.Context, pipeline string) error
}

// DestroyBlocker is the downstream-blocker predicate the destroy op consults before
// tearing a pipeline down (destroy refuses while any registered pipeline declares
// depends_on the target, any downstream run_inputs row names its runs, or any
// outstanding dead-letter entry names it as failed_upstream). Those predicates are
// E10.1's; this seam defaults OPEN so the teardown action is wired now and E10.1
// supplies the guard.
type DestroyBlocker interface {
	// Blocked reports whether the pipeline's teardown is blocked, a human reason when
	// it is, and any error consulting the blocker. The default (openBlocker) never
	// blocks.
	Blocked(ctx context.Context, pipeline string) (blocked bool, reason string, err error)
}

// RunLister lists the prunable runs for a pipeline so destroy can write their
// archival summaries before deleting the run rows.
// The default (noopRunLister) returns no runs, so summaries are a later wiring.
type RunLister interface {
	ListPrunableRuns(ctx context.Context, pipeline string) ([]store.PrunableRun, error)
}

// noopRunLister is the default RunLister: no runs listed, no summaries written
// until the archival tier wires the real reader.
type noopRunLister struct{}

func (noopRunLister) ListPrunableRuns(context.Context, string) ([]store.PrunableRun, error) {
	return nil, nil
}

// openBlocker is the default DestroyBlocker: it never blocks (destroy proceeds). It
// is the honest E03.10 default -- the real downstream predicates arrive with E10.1.
type openBlocker struct{}

func (openBlocker) Blocked(context.Context, string) (bool, string, error) { return false, "", nil }

// noopReverter is the default DataReverter: it reverts nothing. The real
// journal-driven reverse-replay is E06; until then destroy runs the flow with this
// no-op so the seam and its ordering are wired.
type noopReverter struct{}

func (noopReverter) RevertUnpromoted(context.Context, string) error { return nil }

// noopObjectDeleter is the default ObjectDeleter: it deletes no bytes. The real
// content-addressed deletion is E05/E07.
type noopObjectDeleter struct{}

func (noopObjectDeleter) DeleteObjects(context.Context, string) error { return nil }

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

// BlockedError is a destroy refused by the downstream-blocker predicate (E10.1): a
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

// WithDataReverter sets the un-promoted-data revert seam (E06's reverse-replay). A
// nil reverter is ignored, keeping the no-op default.
func WithDataReverter(r DataReverter) DestroyerOption {
	return func(d *Destroyer) {
		if r != nil {
			d.reverter = r
		}
	}
}

// WithObjectDeleter sets the object-store-bytes deletion seam (E05/E07). A nil
// deleter is ignored, keeping the no-op default.
func WithObjectDeleter(o ObjectDeleter) DestroyerOption {
	return func(d *Destroyer) {
		if o != nil {
			d.objects = o
		}
	}
}

// WithDestroyBlocker sets the downstream-blocker predicate seam (E10.1). A nil
// blocker is ignored, keeping the open default (never blocks).
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
	// revert failure leaves meta exactly as it was (E06 fills the reverse-replay body).
	if err := d.reverter.RevertUnpromoted(ctx, name); err != nil {
		return fmt.Errorf("dispatch: destroy pipeline %q: revert un-promoted data: %w", name, err)
	}

	// Archive summaries for remaining runs before the retirement deletes, inside
	// the same transaction so stamps keep resolving.
	var sums []store.RunSummary
	if runs, err := d.runLister.ListPrunableRuns(ctx, name); err == nil && len(runs) > 0 {
		for _, r := range runs {
			sums = append(sums, store.BuildRunSummary(r))
		}
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

	// Free the object-store bytes only after the meta index rows are gone (E05/E07
	// fills the real content-addressed deletion).
	if err := d.objects.DeleteObjects(ctx, name); err != nil {
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
