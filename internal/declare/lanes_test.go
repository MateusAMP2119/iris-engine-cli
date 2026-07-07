package declare_test

import (
	"strings"
	"testing"

	"github.com/MateusAMP2119/iris-engine-cli/internal/declare"
)

// regView builds a RegistryView from a pipeline->membership map and the set of
// lanes whose composer has been applied.
func regView(members map[string]declare.Membership, composed ...string) declare.RegistryView {
	applied := map[string]bool{}
	for _, lane := range composed {
		applied[lane] = true
	}
	return declare.RegistryView{Members: members, ComposersApplied: applied}
}

// member is a small constructor for a registered pipeline's lane membership.
func member(lane string, contained bool) declare.Membership {
	return declare.Membership{Lane: lane, Contained: contained}
}

// TestComposerFileShape proves a lane composer is the lane-folder iris-declare.yaml
// one level above the pipeline folders it sequences, discriminated from a pipeline
// by content: a composer carries lane+order, a pipeline carries run. The composer
// validates as a full-lane rewrite when its order names its immediate member
// folders; the pipeline is recognized as a member, not a lane writer.
//
// spec: S03/composer-file-shape
func TestComposerFileShape(t *testing.T) {
	composerYAML := []byte("lane: ingest\norder:\n  - extract_orders\n  - load_orders\n")
	pipelineYAML := []byte("name: load_orders\nrun: [python, main.py]\n")

	// Discriminated by content: order+lane => composer, run => pipeline.
	cDecl, err := declare.ParseDeclaration(composerYAML)
	if err != nil {
		t.Fatalf("parse composer: %v", err)
	}
	if cDecl.Kind != declare.KindComposer {
		t.Fatalf("composer kind = %v, want composer (carries order+lane)", cDecl.Kind)
	}
	if cDecl.Composer == nil || cDecl.Composer.Lane != "ingest" || len(cDecl.Composer.Order) != 2 {
		t.Fatalf("composer content = %+v, want lane ingest with 2 order entries", cDecl.Composer)
	}
	pDecl, err := declare.ParseDeclaration(pipelineYAML)
	if err != nil {
		t.Fatalf("parse pipeline: %v", err)
	}
	if pDecl.Kind != declare.KindPipeline {
		t.Fatalf("pipeline kind = %v, want pipeline (carries run)", pDecl.Kind)
	}

	// The composer sits in the lane folder one level above the pipeline folders it
	// sequences: its order names its immediate member subfolders.
	eff, err := declare.ValidateComposerApply(declare.RegistryView{}, declare.ComposerApply{
		Lane:          cDecl.Composer.Lane,
		Folder:        "ingest",
		Order:         cDecl.Composer.Order,
		MemberFolders: []string{"extract_orders", "load_orders"},
	})
	if err != nil {
		t.Fatalf("composer one level above its member folders should validate: %v", err)
	}
	if !eff.WritesLanes() || eff.LaneRewrite == nil {
		t.Fatalf("a recognized composer apply must write the lane, got effects %+v", eff)
	}

	// The pipeline declaration, applied one level below inside the lane folder, is a
	// member: it never writes the lane.
	pEff, err := declare.ValidatePipelineApply(regView(nil, "ingest"), declare.PipelineApply{
		Pipeline: pDecl.Pipeline.Name, FolderLane: "ingest",
	})
	if err != nil {
		t.Fatalf("member apply inside the lane folder should validate: %v", err)
	}
	if pEff.WritesLanes() {
		t.Fatalf("a pipeline (member) apply must not write lanes, got %+v", pEff)
	}
}

// TestComposerLaneMatchesFolder proves a composer's lane value must match its lane
// folder name; a mismatch is rejected on apply, an agreement accepted.
//
// spec: S03/composer-lane-matches-folder
func TestComposerLaneMatchesFolder(t *testing.T) {
	tests := []struct {
		name    string
		lane    string
		folder  string
		wantErr bool
	}{
		{"match", "ingest", "ingest", false},
		{"mismatch", "analytics", "ingest", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := declare.ValidateComposerApply(declare.RegistryView{}, declare.ComposerApply{
				Lane:          tc.lane,
				Folder:        tc.folder,
				Order:         []string{"a"},
				MemberFolders: []string{"a"},
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("mismatched composer lane/folder should be rejected")
				}
				if !strings.Contains(err.Error(), tc.lane) || !strings.Contains(err.Error(), tc.folder) {
					t.Errorf("error %q should name both the declared lane and its folder", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("lane matching its folder should validate: %v", err)
			}
		})
	}

	// An empty lane and folder trivially "match" (both blank) but must still be
	// rejected on entry, never written as an empty-keyed lanes roster row.
	t.Run("empty lane and folder", func(t *testing.T) {
		_, err := declare.ValidateComposerApply(declare.RegistryView{}, declare.ComposerApply{
			Lane:          "",
			Folder:        "",
			Order:         []string{"a"},
			MemberFolders: []string{"a"},
		})
		if err == nil {
			t.Fatal("a composer with an empty lane and folder must be rejected, not written as an empty-keyed lane")
		}
	})
}

// TestComposerRequiredAtTwo proves apply rejects a lane reaching 2+ pipelines
// without a composer, while a single-pipeline lane is valid with no composer.
//
// spec: S03/composer-required-at-two
func TestComposerRequiredAtTwo(t *testing.T) {
	t.Run("single member needs no composer", func(t *testing.T) {
		eff, err := declare.ValidatePipelineApply(declare.RegistryView{}, declare.PipelineApply{
			Pipeline: "solo", FolderLane: "solo",
		})
		if err != nil {
			t.Fatalf("a single-pipeline lane is valid without a composer: %v", err)
		}
		if eff.WritesLanes() {
			t.Fatalf("a single-member apply writes no lanes, got %+v", eff)
		}
	})

	t.Run("second member without composer rejected", func(t *testing.T) {
		view := regView(map[string]declare.Membership{"a": member("ingest", true)})
		_, err := declare.ValidatePipelineApply(view, declare.PipelineApply{
			Pipeline: "b", FolderLane: "ingest",
		})
		if err == nil {
			t.Fatal("a lane reaching 2+ pipelines without a composer must be rejected")
		}
		if !strings.Contains(err.Error(), "ingest") {
			t.Errorf("error %q should name the lane", err)
		}
	})
}

// TestInlineContainmentAgree proves that when a pipeline joins a lane both inline
// (lane: X) and by folder containment (folder Y), the two must name the same lane;
// a disagreement is rejected, an agreement accepted.
//
// spec: S03/inline-containment-agree
func TestInlineContainmentAgree(t *testing.T) {
	tests := []struct {
		name    string
		inline  string
		folder  string
		wantErr bool
	}{
		{"agree", "ingest", "ingest", false},
		{"disagree", "analytics", "ingest", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := declare.ValidatePipelineApply(declare.RegistryView{}, declare.PipelineApply{
				Pipeline: "p", InlineLane: tc.inline, FolderLane: tc.folder,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("an inline lane disagreeing with its containing folder must be rejected")
				}
				if !strings.Contains(err.Error(), tc.inline) || !strings.Contains(err.Error(), tc.folder) {
					t.Errorf("error %q should name both the inline lane and the folder", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("inline lane agreeing with its folder should validate: %v", err)
			}
		})
	}
}

// TestOmittedLaneOwnLane proves a pipeline that omits lane is placed in its own
// implicit lane (named for itself, unique), parallel with every other pipeline,
// and writes no lanes.
//
// spec: S03/omitted-lane-own-lane
func TestOmittedLaneOwnLane(t *testing.T) {
	alpha, err := declare.ValidatePipelineApply(declare.RegistryView{}, declare.PipelineApply{Pipeline: "alpha"})
	if err != nil {
		t.Fatalf("a lane-less pipeline should validate: %v", err)
	}
	beta, err := declare.ValidatePipelineApply(declare.RegistryView{}, declare.PipelineApply{Pipeline: "beta"})
	if err != nil {
		t.Fatalf("a lane-less pipeline should validate: %v", err)
	}

	if alpha.Lane != "alpha" {
		t.Errorf("omitted-lane pipeline alpha resolved to lane %q, want its own lane alpha", alpha.Lane)
	}
	if beta.Lane != "beta" {
		t.Errorf("omitted-lane pipeline beta resolved to lane %q, want its own lane beta", beta.Lane)
	}
	// Parallel with everything: two lane-less pipelines occupy distinct implicit
	// lanes, never a shared one.
	if alpha.Lane == beta.Lane {
		t.Errorf("two lane-less pipelines share lane %q; each must get its own implicit lane", alpha.Lane)
	}
	if alpha.WritesLanes() || beta.WritesLanes() {
		t.Error("an implicit own-lane placement writes no lanes")
	}
}

// TestOrderEntriesContained proves apply validates that every composer order entry
// names a pipeline folder contained inside the lane folder; an entry naming a
// non-contained folder is rejected.
//
// spec: S03/order-entries-contained
func TestOrderEntriesContained(t *testing.T) {
	t.Run("all contained", func(t *testing.T) {
		_, err := declare.ValidateComposerApply(declare.RegistryView{}, declare.ComposerApply{
			Lane:          "ingest",
			Folder:        "ingest",
			Order:         []string{"extract_orders", "load_orders"},
			MemberFolders: []string{"extract_orders", "load_orders", "reset_counters"},
		})
		if err != nil {
			t.Fatalf("order naming only contained folders should validate: %v", err)
		}
	})

	t.Run("entry outside the lane folder", func(t *testing.T) {
		_, err := declare.ValidateComposerApply(declare.RegistryView{}, declare.ComposerApply{
			Lane:          "ingest",
			Folder:        "ingest",
			Order:         []string{"extract_orders", "stranger"},
			MemberFolders: []string{"extract_orders", "load_orders"},
		})
		if err == nil {
			t.Fatal("an order entry naming a folder not inside the lane folder must be rejected")
		}
		if !strings.Contains(err.Error(), "stranger") {
			t.Errorf("error %q should name the offending order entry", err)
		}
	})

	t.Run("duplicate order entry", func(t *testing.T) {
		_, err := declare.ValidateComposerApply(declare.RegistryView{}, declare.ComposerApply{
			Lane:          "ingest",
			Folder:        "ingest",
			Order:         []string{"extract_orders", "extract_orders"},
			MemberFolders: []string{"extract_orders", "load_orders"},
		})
		if err == nil {
			t.Fatal("a pipeline listed twice in the order must be rejected; it may not run twice per pass")
		}
		if !strings.Contains(err.Error(), "extract_orders") {
			t.Errorf("error %q should name the duplicated order entry", err)
		}
	})
}

// TestOutsideMemberRejected proves an inline lane: without folder containment is
// valid only while the lane is single-member: an apply that would create a 2+ lane
// with a member outside the lane folder is rejected, with guidance to move the
// folder in.
//
// spec: S03/outside-member-rejected
func TestOutsideMemberRejected(t *testing.T) {
	t.Run("single outside member is nominal", func(t *testing.T) {
		eff, err := declare.ValidatePipelineApply(declare.RegistryView{}, declare.PipelineApply{
			Pipeline: "p", InlineLane: "ingest",
		})
		if err != nil {
			t.Fatalf("a single-member inline lane without containment is valid: %v", err)
		}
		if eff.Lane != "ingest" {
			t.Errorf("inline lane resolved to %q, want ingest", eff.Lane)
		}
	})

	t.Run("outside member growing lane to 2+ rejected", func(t *testing.T) {
		view := regView(map[string]declare.Membership{"inside": member("ingest", true)}, "ingest")
		_, err := declare.ValidatePipelineApply(view, declare.PipelineApply{
			Pipeline: "outsider", InlineLane: "ingest",
		})
		if err == nil {
			t.Fatal("a 2+ lane with a member outside the lane folder must be rejected")
		}
		if !strings.Contains(err.Error(), "ingest") {
			t.Errorf("error %q should name the lane", err)
		}
		if !strings.Contains(err.Error(), "move") {
			t.Errorf("error %q should guide the author to move the folder into the lane", err)
		}
	})
}

// TestPipelineSingleLane proves apply validates that each pipeline belongs to
// exactly one lane; membership in more than one lane is rejected, from both the
// member-apply side and the composer-order side.
//
// spec: S03/pipeline-single-lane
func TestPipelineSingleLane(t *testing.T) {
	t.Run("member re-applied into a different lane", func(t *testing.T) {
		view := regView(map[string]declare.Membership{"p": member("ingest", true)})
		_, err := declare.ValidatePipelineApply(view, declare.PipelineApply{
			Pipeline: "p", InlineLane: "analytics", FolderLane: "analytics",
		})
		if err == nil {
			t.Fatal("a pipeline already in one lane cannot join a second")
		}
		if !strings.Contains(err.Error(), "ingest") || !strings.Contains(err.Error(), "analytics") {
			t.Errorf("error %q should name both lanes", err)
		}
	})

	t.Run("composer ordering a pipeline owned by another lane", func(t *testing.T) {
		view := regView(map[string]declare.Membership{"p": member("analytics", true)})
		_, err := declare.ValidateComposerApply(view, declare.ComposerApply{
			Lane:          "ingest",
			Folder:        "ingest",
			Order:         []string{"p"},
			MemberFolders: []string{"p"},
		})
		if err == nil {
			t.Fatal("a composer cannot order a pipeline already registered in another lane")
		}
		if !strings.Contains(err.Error(), "ingest") || !strings.Contains(err.Error(), "analytics") {
			t.Errorf("error %q should name both lanes", err)
		}
	})
}

// TestTwoPlusInterlock proves a pipeline apply that would leave its lane folder
// with 2+ registered members is rejected, naming the lane, unless that lane's
// composer has been applied.
//
// spec: S06.3/two-plus-interlock
func TestTwoPlusInterlock(t *testing.T) {
	base := map[string]declare.Membership{"first": member("ingest", true)}

	t.Run("2+ without composer rejected naming the lane", func(t *testing.T) {
		_, err := declare.ValidatePipelineApply(regView(base), declare.PipelineApply{
			Pipeline: "second", FolderLane: "ingest",
		})
		if err == nil {
			t.Fatal("applying a second member without an applied composer must be rejected")
		}
		if !strings.Contains(err.Error(), "ingest") {
			t.Errorf("interlock error %q must name the lane", err)
		}
	})

	t.Run("2+ allowed once the composer is applied", func(t *testing.T) {
		eff, err := declare.ValidatePipelineApply(regView(base, "ingest"), declare.PipelineApply{
			Pipeline: "second", FolderLane: "ingest",
		})
		if err != nil {
			t.Fatalf("a second member is allowed once the composer is applied: %v", err)
		}
		if eff.WritesLanes() {
			t.Errorf("even joining a composed lane, a member apply writes no lanes: %+v", eff)
		}
	})

	t.Run("own-lane colliding into a populated lane still hits the interlock", func(t *testing.T) {
		// A lane-less pipeline is not a containment member of anything; if its
		// implicit own-lane name collides with an already-populated lane, the apply
		// still leaves that lane with 2+ members and must be rejected, never merged.
		view := regView(map[string]declare.Membership{"first": member("collide", true)})
		_, err := declare.ValidatePipelineApply(view, declare.PipelineApply{Pipeline: "collide"})
		if err == nil {
			t.Fatal("a lane-less pipeline colliding into a populated lane must be rejected, not silently merged")
		}
		if !strings.Contains(err.Error(), "collide") {
			t.Errorf("error %q should name the colliding lane", err)
		}
	})
}

// TestMemberApplyNeverWritesLanes proves a member pipeline apply never writes
// lanes: a pipeline's position always comes from the composer's apply, which
// carries the full-lane rewrite. Asserted structurally on the effects type.
//
// spec: S06.3/member-apply-never-writes-lanes
func TestMemberApplyNeverWritesLanes(t *testing.T) {
	pipelineApplies := []struct {
		name  string
		view  declare.RegistryView
		apply declare.PipelineApply
	}{
		{"own lane", declare.RegistryView{}, declare.PipelineApply{Pipeline: "solo"}},
		{"single folder lane", declare.RegistryView{}, declare.PipelineApply{Pipeline: "a", FolderLane: "lane"}},
		{
			"joins a composed lane",
			regView(map[string]declare.Membership{"a": member("ingest", true)}, "ingest"),
			declare.PipelineApply{Pipeline: "b", FolderLane: "ingest"},
		},
	}
	for _, tc := range pipelineApplies {
		t.Run("pipeline/"+tc.name, func(t *testing.T) {
			eff, err := declare.ValidatePipelineApply(tc.view, tc.apply)
			if err != nil {
				t.Fatalf("apply should validate: %v", err)
			}
			if eff.WritesLanes() || eff.LaneRewrite != nil {
				t.Fatalf("a member apply must carry no lane-position write, got %+v", eff)
			}
		})
	}

	t.Run("composer/full-lane rewrite", func(t *testing.T) {
		order := []string{"extract_orders", "load_orders"}
		eff, err := declare.ValidateComposerApply(declare.RegistryView{}, declare.ComposerApply{
			Lane:          "ingest",
			Folder:        "ingest",
			Order:         order,
			MemberFolders: order,
		})
		if err != nil {
			t.Fatalf("composer apply should validate: %v", err)
		}
		if !eff.WritesLanes() || eff.LaneRewrite == nil {
			t.Fatalf("a composer apply must carry the full-lane rewrite, got %+v", eff)
		}
		if eff.LaneRewrite.Lane != "ingest" || !equalSlices(eff.LaneRewrite.Order, order) {
			t.Errorf("lane rewrite = %+v, want lane ingest with order %v written whole", eff.LaneRewrite, order)
		}
	})
}
