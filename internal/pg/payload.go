package pg

import (
	"fmt"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// This file owns the mode-aware payload-tier policy the always-on capture path
// applies (specification sections 1, 4, 12 and 14). Capture is unconditional: every
// write to a declared table is stamped in the journal regardless of the pipeline's
// artifact or data mode. What the mode governs is the payload TIER of that stamp,
// never whether the stamp is written:
//
//   - Full pre-image (the prior row JSON) is kept exactly where undo can spend it: a
//     wipe-eligible (disposable, un-promoted) update or delete. That is the dev loop's
//     revert budget -- a wipe restores those pre-images (spec section 14: "full
//     pre-images only where undo spends them (dev loop)").
//   - A slim stamp (no pre-image) is recorded everywhere else: on every insert (a
//     wipe reverts an insert by deleting the row, no image needed), and on every
//     write born promoted -- a permanent-mode write, which is not wipe-eligible.
//
// The trigger (capture.go) learns the write's wipe-eligibility in-transaction from
// the per-session setting iris.wipe_eligible (WipeEligibleSetting), which the engine
// injects on the run's data connection at spawn, mirroring how iris.run_id rides the
// same connection (runid.go). This file holds the pure, testable model of that
// decision so the runtime trigger and the offline classification agree by
// construction.
//
// The durability gate (PermanentRequiresBuilt) is the other half of the mode policy:
// permanent data requires a built artifact, so the engine refuses a permanent-mode
// write from an un-built source pipeline (spec section 12: "loose source never writes
// permanent data").

// WipeEligibleSetting is the name of the per-session Postgres configuration setting (a
// custom, dotted-namespace GUC) that carries a write's wipe-eligibility on its injected
// data connection: 'on' for disposable (wipe-eligible) data, 'off' for permanent
// (born-promoted) data. The capture trigger reads it in-transaction with
// current_setting to decide the payload tier (capture.go). Engine-owned and stable; it
// mirrors RunIDSetting, set on the same injected connection at spawn.
//
// When the setting is absent, the trigger treats the write as wipe-eligible: an
// unattributed data mode is the registration default, which is disposable
// (WipeEligible(DataDisposable) is true), so the two layers agree.
const WipeEligibleSetting = "iris.wipe_eligible"

// PayloadTier is the tier of the provenance stamp capture writes for one changed row:
// a full pre-image or a slim (image-less) stamp.
type PayloadTier string

// The two payload tiers.
const (
	// PayloadFull is a stamp carrying the full prior row as its pre_image: kept only
	// on a wipe-eligible update or delete, where undo can spend it.
	PayloadFull PayloadTier = "full"
	// PayloadSlim is an image-less stamp (pre_image null): every insert, and every
	// write born promoted (permanent mode). Provenance is preserved; no row copy.
	PayloadSlim PayloadTier = "slim"
)

// WriteOp is a captured write operation. Its values are the data_journal.op value set
// (specification section 4), mirrored here so the tier model and the journal agree.
type WriteOp string

// The captured write operations.
const (
	// OpInsert is a row insert; its stamp never carries a pre-image (a wipe reverts it
	// by deleting the row).
	OpInsert WriteOp = "insert"
	// OpUpdate is a row update; wipe-eligible updates carry the full prior row.
	OpUpdate WriteOp = "update"
	// OpDelete is a row delete; wipe-eligible deletes carry the full prior row.
	OpDelete WriteOp = "delete"
)

// WipeEligible reports whether writes made under the given data mode are wipe-eligible:
// only disposable data is (a permanent-mode write is born promoted and out of wipe
// scope). Changing a pipeline's data mode changes exactly this -- the wipe eligibility
// of its future writes -- and nothing about whether those writes are captured
// (specification section 12).
func WipeEligible(mode declare.DataMode) bool {
	return mode == declare.DataDisposable
}

// ClassifyPayloadTier returns the tier of the stamp capture writes for one changed row
// under the given data mode and operation (specification sections 4 and 14). It is the
// pure model of the decision the iris.capture() trigger makes in-transaction: a full
// pre-image exactly on a wipe-eligible update or delete, a slim stamp everywhere else
// (every insert, and every write born promoted under a permanent data mode). It never
// returns "no stamp": capture is unconditional, so every op in every mode classifies to
// a tier -- only the tier differs.
func ClassifyPayloadTier(mode declare.DataMode, op WriteOp) PayloadTier {
	if WipeEligible(mode) && (op == OpUpdate || op == OpDelete) {
		return PayloadFull
	}
	return PayloadSlim
}

// PermanentRequiresBuilt enforces the data-durability gate (specification sections 1
// and 12): permanent data requires a built artifact, so a permanent-mode write from an
// un-built source pipeline is refused. built reports whether the writing pipeline's
// artifact is built (a content-addressed binary with a recorded hash). A disposable
// write is admitted from either artifact mode; only the permanent + un-built
// combination is refused. It returns nil when the write is durability-admissible and a
// descriptive error naming both the permanent mode and the built requirement otherwise.
func PermanentRequiresBuilt(mode declare.DataMode, built bool) error {
	if mode == declare.DataPermanent && !built {
		return fmt.Errorf("iris: refused permanent-mode write from un-built source; permanent data requires a built artifact")
	}
	return nil
}
