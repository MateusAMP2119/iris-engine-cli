package declare

import (
	"errors"
	"fmt"
	"sort"
)

// This file owns the folder-surface rules (#192): member access must be a subset of the composer's surface, declared writes are table-exclusive among siblings, and undeclared members inherit the surface minus sibling write claims.

// HasSurface reports whether the composer declares a folder surface at all.
func (c *Composer) HasSurface() bool {
	return c != nil && (len(c.Reads) > 0 || len(c.Writes) > 0)
}

// ValidateSurface checks the composer's own reads/writes entries against the same shape rules a pipeline's entries obey.
func ValidateSurface(c *Composer) error {
	var errs []error
	for i, a := range c.Reads {
		if err := validateAccessEntry("reads", i, a); err != nil {
			errs = append(errs, err)
		}
	}
	for i, a := range c.Writes {
		if err := validateAccessEntry("writes", i, a); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ValidateMemberSurface checks one member against the folder surface (subset both sides) and its siblings' exclusive write claims; no surface validates nothing.
func ValidateMemberSurface(c *Composer, member *Pipeline, siblings []*Pipeline) error {
	if !c.HasSurface() {
		return nil
	}
	var errs []error
	errs = append(errs, subsetErrors("reads", member.Name, member.Reads, c.Reads)...)
	errs = append(errs, subsetErrors("writes", member.Name, member.Writes, c.Writes)...)
	for _, sib := range siblings {
		if sib == nil || sib.Name == member.Name {
			continue
		}
		for _, w := range member.Writes {
			if accessHasTable(sib.Writes, w.Table) {
				errs = append(errs, fmt.Errorf("declare: pipeline %q writes table %q, already claimed by sibling %q; declared writes are exclusive within the folder", member.Name, w.Table, sib.Name))
			}
		}
	}
	return errors.Join(errs...)
}

// ValidateComposerSurface checks a whole folder at composer apply: surface shape, every member's subset, and pairwise write-table claims; members absent from the map are left to their own apply.
func ValidateComposerSurface(c *Composer, members map[string]*Pipeline) error {
	if !c.HasSurface() {
		return nil
	}
	if err := ValidateSurface(c); err != nil {
		return err
	}
	var errs []error
	names := make([]string, 0, len(members))
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		member := members[name]
		if member == nil {
			continue
		}
		var siblings []*Pipeline
		for _, other := range names {
			if other != name {
				siblings = append(siblings, members[other])
			}
		}
		if err := ValidateMemberSurface(c, member, siblings); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// EffectiveAccess derives the member's effective reads/writes: declared sides stand, undeclared reads inherit the surface whole, undeclared writes inherit the surface minus sibling-claimed tables.
func EffectiveAccess(c *Composer, member *Pipeline, siblings []*Pipeline) (reads, writes []Access) {
	if !c.HasSurface() {
		return append([]Access(nil), member.Reads...), append([]Access(nil), member.Writes...)
	}
	if len(member.Reads) > 0 {
		reads = append([]Access(nil), member.Reads...)
	} else {
		reads = append([]Access(nil), c.Reads...)
	}
	if len(member.Writes) > 0 {
		writes = append([]Access(nil), member.Writes...)
		return reads, writes
	}
	claimed := map[string]bool{}
	for _, sib := range siblings {
		if sib == nil || sib.Name == member.Name {
			continue
		}
		for _, w := range sib.Writes {
			claimed[w.Table] = true
		}
	}
	for _, w := range c.Writes {
		if !claimed[w.Table] {
			writes = append(writes, w)
		}
	}
	return reads, writes
}

// subsetErrors reports member entries outside the surface's matching side: absent table or fields beyond the surface entry's.
func subsetErrors(side, name string, member, surface []Access) []error {
	var errs []error
	for _, a := range member {
		s, ok := findAccess(surface, a.Table)
		if !ok {
			errs = append(errs, fmt.Errorf("declare: pipeline %q %s table %q is outside the folder surface; the composer's %s must include it", name, side, a.Table, side))
			continue
		}
		allowed := map[string]bool{}
		for _, f := range s.Fields {
			allowed[f] = true
		}
		for _, f := range a.Fields {
			if !allowed[f] {
				errs = append(errs, fmt.Errorf("declare: pipeline %q %s table %q field %q is outside the folder surface's fields for that table", name, side, a.Table, f))
			}
		}
	}
	return errs
}

// findAccess returns the surface entry for table, if any.
func findAccess(list []Access, table string) (Access, bool) {
	for _, a := range list {
		if a.Table == table {
			return a, true
		}
	}
	return Access{}, false
}

// accessHasTable reports whether list carries an entry for table.
func accessHasTable(list []Access, table string) bool {
	_, ok := findAccess(list, table)
	return ok
}
