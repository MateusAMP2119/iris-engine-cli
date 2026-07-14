package declare

import (
	"fmt"
	"sort"
)

// Membership is a registered pipeline's lane placement in the registry view: the
// lane it belongs to, and whether its folder sits inside that lane's folder (a
// containment member) as opposed to an outside inline member (lane: by name only).
// An outside member is valid only while its lane stays single-member.
type Membership struct {
	// Lane is the lane the registered pipeline belongs to.
	Lane string
	// Contained reports whether the pipeline's folder is inside the lane folder.
	// A pipeline that names a lane inline without living in that lane's folder is
	// not contained; it may only remain while its lane is single-member.
	Contained bool
}

// RegistryView is the already-registered lane state a single `iris declare apply`
// validates against: which lane each registered pipeline belongs to, and which
// lanes have had their composer applied. Apply is single-file and upstream-first,
// so validation is pure over this snapshot; the
// persistence layer (a later task) fills it from meta. Both maps are read-only
// here and a nil map reads as empty.
type RegistryView struct {
	// Members maps each registered pipeline name to its lane membership.
	Members map[string]Membership
	// ComposersApplied is the set of lanes whose composer apply has landed; a lane
	// present with value true has had its whole order written to the lanes roster.
	ComposersApplied map[string]bool
}

// composerApplied reports whether the given lane's composer has been applied.
func (v RegistryView) composerApplied(lane string) bool {
	return v.ComposersApplied[lane]
}

// PipelineApply is the lane-relevant shape of a single pipeline `declare apply`:
// the pipeline's name, its inline lane declaration, and the lane folder that
// physically contains it. A pipeline joins a lane inline (InlineLane), by
// containment (FolderLane), or both; an empty field means that signal is absent.
type PipelineApply struct {
	// Pipeline is the applying pipeline's name.
	Pipeline string
	// InlineLane is the declaration's lane field; empty when omitted.
	InlineLane string
	// FolderLane is the name of the lane folder physically containing the pipeline
	// folder; empty when the pipeline is not inside a lane folder (an inline lane
	// joined by name only, or a lane-less pipeline in its own implicit lane).
	FolderLane string
}

// ComposerApply is the lane-relevant shape of a single composer `declare apply`.
// The composer file sits in the lane folder, one level above the pipeline folders
// it sequences; MemberFolders are those immediate pipeline subfolders.
type ComposerApply struct {
	// Lane is the composer's declared lane (its lane field).
	Lane string
	// Folder is the name of the lane folder the composer file sits in.
	Folder string
	// Order is the lane's serial walk: member pipeline names in order.
	Order []string
	// MemberFolders are the pipeline folder names one level below the lane folder.
	MemberFolders []string
}

// LaneRewrite is the whole-lane order a composer apply writes to the lanes roster,
// replacing that lane's prior order atomically (written whole only by the
// composer's own apply).
type LaneRewrite struct {
	// Lane is the lane whose order is rewritten.
	Lane string
	// Order is the complete member sequence written for the lane.
	Order []string
}

// Effects is the lane-persistence outcome of a validated apply. Lane is the lane
// the apply resolves its subject to (informational, not itself a write).
// LaneRewrite is set only for a composer apply, which rewrites its lane's whole
// order; a member (pipeline) apply leaves it nil, because a pipeline's lane
// position always comes from the composer's apply, never a member apply.
type Effects struct {
	// Lane is the lane the applied subject resolves to.
	Lane string
	// LaneRewrite, when non-nil, is the full-lane order a composer apply writes.
	LaneRewrite *LaneRewrite
}

// WritesLanes reports whether these effects write the lanes roster. It is true
// exactly for a composer apply's full-lane rewrite; a member apply never writes
// lanes.
func (e Effects) WritesLanes() bool {
	return e.LaneRewrite != nil
}

// ValidatePipelineApply validates a single pipeline `declare apply` against the
// registry view and returns its lane effects. It enforces the pipeline-side lane
// rules: inline and containment lanes must
// agree; a pipeline belongs to exactly one lane; an inline lane joined without
// containment is valid only while single-member; a lane reaching 2+ members needs
// its composer applied (the 2+ interlock). A pipeline (member) apply never writes
// lanes, so the returned effects carry no lane rewrite.
func ValidatePipelineApply(view RegistryView, apply PipelineApply) (Effects, error) {
	if apply.Pipeline == "" {
		return Effects{}, fmt.Errorf("declare: pipeline apply has no name")
	}

	lane, contained, err := resolveLane(apply)
	if err != nil {
		return Effects{}, err
	}

	// A pipeline belongs to exactly one lane: a re-apply may not move it, and no
	// two lanes may claim it.
	if existing, ok := view.Members[apply.Pipeline]; ok && existing.Lane != lane {
		return Effects{}, fmt.Errorf("declare: pipeline %q is already registered in lane %q; it cannot also join lane %q; a pipeline belongs to exactly one lane", apply.Pipeline, existing.Lane, lane)
	}

	// Determine the lane's membership after this apply, and which members sit
	// outside the lane folder.
	count := 1
	var outside []string
	if !contained {
		outside = append(outside, apply.Pipeline)
	}
	for name, m := range view.Members {
		if name == apply.Pipeline || m.Lane != lane {
			continue
		}
		count++
		if !m.Contained {
			outside = append(outside, name)
		}
	}

	if count >= 2 {
		// A 2+ lane may hold no member outside its folder: the inline-without-
		// containment placement is nominal only while single-member.
		if len(outside) > 0 {
			sort.Strings(outside)
			return Effects{}, fmt.Errorf("declare: applying pipeline %q would grow lane %q to %d members while %v sit outside the lane folder; move each into the %q folder so containment matches the lane", apply.Pipeline, lane, count, outside, lane)
		}
		// The 2+ interlock: a lane of 2+ registered members needs its composer
		// applied first.
		if !view.composerApplied(lane) {
			return Effects{}, fmt.Errorf("declare: applying pipeline %q would leave lane %q with %d registered members; apply the lane %q composer before its second member", apply.Pipeline, lane, count, lane)
		}
	}

	// A member apply never writes lanes.
	return Effects{Lane: lane}, nil
}

// resolveLane resolves the lane a pipeline apply joins and whether it is contained
// in that lane's folder, enforcing inline/containment agreement. The returned
// contained is honest to Membership.Contained: true only when the pipeline folder
// sits inside the lane folder. A pipeline that omits lane and has no containing
// lane folder is placed in its own implicit lane, named for itself and parallel
// with everything; it has no lane folder to sit inside,
// so contained is false. Because that implicit lane is named for the pipeline, its
// membership stays a single pipeline unless the name collides with an already
// populated lane, in which case the 2+ rules below reject the collision rather
// than silently merging it.
func resolveLane(apply PipelineApply) (lane string, contained bool, err error) {
	switch {
	case apply.InlineLane != "" && apply.FolderLane != "":
		if apply.InlineLane != apply.FolderLane {
			return "", false, fmt.Errorf("declare: pipeline %q declares lane %q but sits in lane folder %q; an inline lane and its containing folder must name the same lane", apply.Pipeline, apply.InlineLane, apply.FolderLane)
		}
		return apply.InlineLane, true, nil
	case apply.InlineLane != "":
		// Inline lane joined by name only, with no containing lane folder: an
		// outside member, valid only while the lane stays single-member.
		return apply.InlineLane, false, nil
	case apply.FolderLane != "":
		return apply.FolderLane, true, nil
	default:
		// Omitted lane, no containing lane folder: its own implicit lane, named for
		// itself. There is no lane folder to be inside, so it is not a containment
		// member.
		return apply.Pipeline, false, nil
	}
}

// ValidateComposerApply validates a single composer `declare apply` against the
// registry view and returns its lane effects. It enforces the composer-side rules:
// the composer's lane must match its folder
// name; every order entry must name a pipeline folder inside the lane folder; and
// no ordered pipeline may already belong to another lane. A composer apply rewrites
// the lane's whole order, so the returned effects carry the full-lane rewrite.
func ValidateComposerApply(view RegistryView, apply ComposerApply) (Effects, error) {
	// A composer must name a real lane and folder: an empty pair trivially
	// "matches" (both blank) but would be written as an empty-keyed lanes row, so
	// it is rejected on entry (mirrors ValidatePipelineApply's name guard).
	if apply.Lane == "" {
		return Effects{}, fmt.Errorf("declare: composer apply has no lane name")
	}
	if apply.Folder == "" {
		return Effects{}, fmt.Errorf("declare: composer apply has no folder name")
	}
	// The composer's lane must match its folder name.
	if apply.Lane != apply.Folder {
		return Effects{}, fmt.Errorf("declare: composer declares lane %q but sits in folder %q; a composer's lane must match its folder name", apply.Lane, apply.Folder)
	}

	folders := make(map[string]bool, len(apply.MemberFolders))
	for _, f := range apply.MemberFolders {
		folders[f] = true
	}
	seen := make(map[string]bool, len(apply.Order))
	for _, name := range apply.Order {
		// Membership by containment: every order entry names a pipeline folder
		// inside the lane folder.
		if !folders[name] {
			return Effects{}, fmt.Errorf("declare: composer for lane %q orders %q, which is not a pipeline folder inside the lane folder; every order entry must name a folder contained in the lane", apply.Lane, name)
		}
		// The order is a serial walk with no repeats: a pipeline listed twice would
		// run twice per pass (one goroutine per lane).
		if seen[name] {
			return Effects{}, fmt.Errorf("declare: composer for lane %q orders pipeline %q more than once; a pipeline appears in the lane order at most once", apply.Lane, name)
		}
		seen[name] = true
		// Each pipeline belongs to exactly one lane.
		if existing, ok := view.Members[name]; ok && existing.Lane != apply.Lane {
			return Effects{}, fmt.Errorf("declare: composer for lane %q orders pipeline %q, which is already registered in lane %q; a pipeline belongs to exactly one lane", apply.Lane, name, existing.Lane)
		}
	}

	// A composer apply rewrites the lane's whole order atomically. Copy the order
	// so the effect never aliases the caller's slice.
	order := append([]string(nil), apply.Order...)
	return Effects{Lane: apply.Lane, LaneRewrite: &LaneRewrite{Lane: apply.Lane, Order: order}}, nil
}
