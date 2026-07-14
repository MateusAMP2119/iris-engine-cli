package pg_test

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
	"github.com/MateusAMP2119/iris-engine-cli/internal/pg"
)

// allOps is the closed set of captured write operations, the op axis of the
// payload-tier matrix.
var allOps = []pg.WriteOp{pg.OpInsert, pg.OpUpdate, pg.OpDelete}

// TestCaptureCoversAllModes proves capture is active in every artifact/data mode
// combination, with only the captured payload TIER differing between modes: capture
// covers all modes, only payload differs.
//
// The integration surface is twofold: the capture-install DDL (the mode-independent
// trigger set the engine emits for every declared table) and the runtime tier
// decision iris.capture() makes per write (modeled by ClassifyPayloadTier). The
// install carries no mode input at all -- so capture is unconditionally on for
// every mode -- and the only thing that varies across modes is whether a
// wipe-eligible update/delete keeps a full pre-image or records a slim stamp.
func TestCaptureCoversAllModes(t *testing.T) {
	// --- Capture install is mode-independent: it is emitted for every declared
	// table unconditionally, and the plan carries no mode input at all (PlanProvision
	// is driven by the declared tables, never a pipeline's artifact or data mode), so
	// capture is active in every mode combination by construction. ---
	tables := discoverGoldenSchemas(t)
	plan, err := pg.PlanProvision(tables, emptyLive(), freshLedgers(tables))
	if err != nil {
		t.Fatalf("PlanProvision: %v", err)
	}
	for _, tp := range plan.Tables {
		if len(tp.CaptureTriggers) != 3 {
			t.Errorf("%s.%s got %d capture triggers, want the full unconditional set of 3 (capture active in every mode)",
				tp.Schema, tp.Table, len(tp.CaptureTriggers))
		}
	}
	// The tier is a RUNTIME decision, not an install-time branch: the one installed
	// capture function serves every mode because it reads the write's wipe-eligibility
	// in-transaction from the per-session setting. There is no per-mode install.
	if fn := pg.CaptureFunctionDDL(); !strings.Contains(fn, "current_setting('"+pg.WipeEligibleSetting) {
		t.Errorf("capture function does not read the per-session %s setting; the payload tier must be a runtime decision so one install covers every mode", pg.WipeEligibleSetting)
	}

	// --- The tier matrix: artifact (source/built) x data (disposable/permanent). The
	// blocked source+permanent combination is durability-refused (it never writes);
	// every valid combination captures every op, and the tier differs only between
	// disposable (wipe-eligible) and permanent. ---
	combos := []struct {
		name  string
		built bool
		mode  declare.DataMode
		valid bool
	}{
		{"source+disposable", false, declare.DataDisposable, true},
		{"built+disposable", true, declare.DataDisposable, true},
		{"built+permanent", true, declare.DataPermanent, true},
		{"source+permanent", false, declare.DataPermanent, false},
	}

	for _, c := range combos {
		durErr := pg.PermanentRequiresBuilt(c.mode, c.built)
		if c.valid && durErr != nil {
			t.Errorf("%s: durability refused a valid combination: %v", c.name, durErr)
		}
		if !c.valid && durErr == nil {
			t.Errorf("%s: durability admitted the blocked source+permanent combination", c.name)
		}
		if !c.valid {
			// The blocked combination never writes, so there is no captured tier to
			// assert; it is refused above.
			continue
		}
		for _, op := range allOps {
			tier := pg.ClassifyPayloadTier(c.mode, op)
			// Capture is active in every valid mode: every op yields a tier, never
			// an absent/empty classification.
			if tier != pg.PayloadFull && tier != pg.PayloadSlim {
				t.Errorf("%s op=%s: capture produced no tier %q; capture must be active in every mode", c.name, op, tier)
			}
			// The tier depends only on the DATA mode (wipe eligibility) and the op,
			// never on the artifact mode: a full pre-image exactly on a wipe-eligible
			// update/delete, a slim stamp everywhere else.
			wantFull := c.mode == declare.DataDisposable && (op == pg.OpUpdate || op == pg.OpDelete)
			want := pg.PayloadSlim
			if wantFull {
				want = pg.PayloadFull
			}
			if tier != want {
				t.Errorf("%s op=%s: tier=%s, want %s", c.name, op, tier, want)
			}
		}
	}

	// The ONLY difference between the two data modes is the tier of a wipe-eligible
	// update/delete: for the same artifact mode, disposable keeps a full pre-image
	// where permanent records a slim stamp, and inserts match in both.
	for _, op := range allOps {
		disp := pg.ClassifyPayloadTier(declare.DataDisposable, op)
		perm := pg.ClassifyPayloadTier(declare.DataPermanent, op)
		if op == pg.OpInsert {
			if disp != perm {
				t.Errorf("insert tier differs by data mode (disposable=%s permanent=%s); only wipe-eligible update/delete may differ", disp, perm)
			}
			continue
		}
		if disp != pg.PayloadFull || perm != pg.PayloadSlim {
			t.Errorf("op=%s: want disposable=full permanent=slim, got disposable=%s permanent=%s", op, disp, perm)
		}
	}
}

// TestModeChangeWipeEligibilityOnly proves changing a pipeline's data mode changes
// the wipe eligibility of its data (and hence the payload tier of a future
// update/delete) but never changes capture behavior: every op is still captured in
// both modes -- a data mode change moves wipe eligibility, never capture.
func TestModeChangeWipeEligibilityOnly(t *testing.T) {
	// Wipe eligibility flips with the data mode.
	if !pg.WipeEligible(declare.DataDisposable) {
		t.Error("disposable data must be wipe-eligible")
	}
	if pg.WipeEligible(declare.DataPermanent) {
		t.Error("permanent data must not be wipe-eligible (born promoted)")
	}

	// The tier of a wipe-eligible update/delete follows the mode change: full while
	// disposable, slim once permanent.
	if got := pg.ClassifyPayloadTier(declare.DataDisposable, pg.OpUpdate); got != pg.PayloadFull {
		t.Errorf("disposable update tier=%s, want full", got)
	}
	if got := pg.ClassifyPayloadTier(declare.DataPermanent, pg.OpUpdate); got != pg.PayloadSlim {
		t.Errorf("permanent update tier=%s, want slim", got)
	}

	// Capture behavior is unchanged by the mode: every op yields a stamp tier in
	// BOTH modes -- the mode never turns capture off, only re-tiers a wipe-eligible
	// update/delete. Inserts are a slim stamp in either mode.
	for _, mode := range []declare.DataMode{declare.DataDisposable, declare.DataPermanent} {
		for _, op := range allOps {
			tier := pg.ClassifyPayloadTier(mode, op)
			if tier != pg.PayloadFull && tier != pg.PayloadSlim {
				t.Errorf("mode=%s op=%s: capture produced no tier; the data mode never disables capture", mode, op)
			}
		}
		if got := pg.ClassifyPayloadTier(mode, pg.OpInsert); got != pg.PayloadSlim {
			t.Errorf("mode=%s insert tier=%s, want slim in every mode", mode, got)
		}
	}
}

// TestUnbuiltPermanentWriteRefused proves the engine refuses permanent-mode data
// writes from a pipeline running un-built source: permanent data requires a built
// artifact, and a loose source never writes permanent data.
func TestUnbuiltPermanentWriteRefused(t *testing.T) {
	// Permanent + un-built source is the one refused combination.
	err := pg.PermanentRequiresBuilt(declare.DataPermanent, false)
	if err == nil {
		t.Fatal("permanent write from un-built source was admitted; it must be refused")
	}
	if msg := strings.ToLower(err.Error()); !strings.Contains(msg, "permanent") || !strings.Contains(msg, "built") {
		t.Errorf("refusal error %q should name both the permanent mode and the built requirement", err.Error())
	}

	// Every other combination is admitted: permanent requires built, but disposable
	// writes freely from either artifact mode, and built+permanent is production.
	for _, ok := range []struct {
		name  string
		mode  declare.DataMode
		built bool
	}{
		{"built+permanent (production)", declare.DataPermanent, true},
		{"source+disposable (dev)", declare.DataDisposable, false},
		{"built+disposable (throwaway-data test)", declare.DataDisposable, true},
	} {
		if err := pg.PermanentRequiresBuilt(ok.mode, ok.built); err != nil {
			t.Errorf("%s: durability refused a valid write: %v", ok.name, err)
		}
	}
}
