package tui

import (
	"sort"
	"strings"
	"unicode"
)

// This file is the `iris ps` search overlay's state and matcher: a fuzzy
// subsequence match over every navigable entity the poll already holds --
// lane names, pipeline names, run ids and states -- narrowing per keystroke,
// no extra route. The overlay's floating layout is rendered in psvrender.go;
// here live the pure parts (candidates, scoring, key routing) so search is
// unit-testable without a terminal.

// psHitKind names what a search hit is, so results can label their kind and
// Enter knows which screen to jump to.
type psHitKind int

// The searchable entity kinds.
const (
	psHitLane psHitKind = iota
	psHitPipeline
	psHitRun
)

// kindTag is the one-letter kind column of the results list.
func (k psHitKind) kindTag() string {
	switch k {
	case psHitLane:
		return "LANE"
	case psHitPipeline:
		return "PIPELINE"
	default:
		return "RUN"
	}
}

// psHit is one search result: its kind, the breadcrumb coordinates Enter
// jumps to, and the label the results list shows.
type psHit struct {
	kind     psHitKind
	lane     string
	pipeline string
	runID    string
	label    string
	score    int
}

// psSearch is the open overlay's state: the typed query, the scored hits
// (best first; the results list renders them bottom-anchored so the best hit
// sits nearest the prompt), and the selection index into hits.
type psSearch struct {
	query []rune
	hits  []psHit
	sel   int
}

// openSearch opens the overlay with an empty query (every entity matches).
func (m *psModel) openSearch() {
	m.search = &psSearch{}
	m.search.rematch(m.snap)
}

// updateSearch routes a keypress while the overlay is open: typing edits the
// query (j/k are literal characters here), arrows move the selection, Enter
// jumps to the selected hit, Esc closes -- the only screen where Esc acts.
func (m *psModel) updateSearch(k psKey) {
	s := m.search
	switch k.kind {
	case psKeyCtrlC:
		m.quit = true
	case psKeyEsc:
		m.search = nil
	case psKeyRune:
		s.query = append(s.query, k.r)
		s.rematch(m.snap)
	case psKeyBackspace:
		if len(s.query) > 0 {
			s.query = s.query[:len(s.query)-1]
			s.rematch(m.snap)
		}
	case psKeyUp:
		if s.sel < len(s.hits)-1 {
			s.sel++
		}
	case psKeyDown:
		if s.sel > 0 {
			s.sel--
		}
	case psKeyEnter:
		if s.sel < len(s.hits) {
			m.jumpTo(s.hits[s.sel])
		}
		m.search = nil
	}
}

// jumpTo lands on the hit: a lane selects and unfolds its rail row, a
// pipeline selects its rail row and focuses the runs table, a run selects its
// pipeline and pins the logs target on it.
func (m *psModel) jumpTo(h psHit) {
	m.expanded[h.lane] = true
	switch h.kind {
	case psHitLane:
		m.selectTree(psTreeRow{lane: h.lane})
		m.pane = psPaneLanes
	case psHitPipeline:
		m.selectTree(psTreeRow{lane: h.lane, pipeline: h.pipeline})
		m.pane = psPaneTable
	case psHitRun:
		m.selectTree(psTreeRow{lane: h.lane, pipeline: h.pipeline})
		m.tblRun = h.runID
		m.pinnedRun = h.runID
		m.pane = psPaneLogs
	}
}

// searchCandidates enumerates every navigable entity in the snapshot: one
// pass over the listing and one over the run rows (never a per-lane re-scan
// of the history -- rematch runs on every keystroke and every poll).
func searchCandidates(s Snapshot) []psHit {
	var out []psHit
	lanes := map[string]bool{}
	pipelines := map[string]bool{}
	addLane := func(name string) {
		if !lanes[name] {
			lanes[name] = true
			out = append(out, psHit{kind: psHitLane, lane: name, label: name})
		}
	}
	addPipeline := func(lane, name string) {
		if !pipelines[name] {
			pipelines[name] = true
			out = append(out, psHit{kind: psHitPipeline, lane: lane, pipeline: name, label: name})
		}
	}
	for _, p := range s.Pipelines {
		addLane(laneOf(p))
		addPipeline(laneOf(p), p.Name)
	}
	for _, run := range s.Ps.Runs {
		addLane(runLaneOf(run))
		addPipeline(runLaneOf(run), run.Pipeline)
		out = append(out, psHit{
			kind:     psHitRun,
			lane:     runLaneOf(run),
			pipeline: run.Pipeline,
			runID:    run.ID,
			label:    run.ID + " · " + run.State,
		})
	}
	return out
}

// rematch rescores every candidate against the current query, keeping matches
// best first (score, then shorter label, then lexical) and snapping the
// selection back to the best hit.
func (s *psSearch) rematch(snap Snapshot) {
	q := string(s.query)
	var hits []psHit
	for _, c := range searchCandidates(snap) {
		if score, ok := fuzzyScore(q, c.label); ok {
			c.score = score
			hits = append(hits, c)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		if len(hits[i].label) != len(hits[j].label) {
			return len(hits[i].label) < len(hits[j].label)
		}
		return hits[i].label < hits[j].label
	})
	s.hits = hits
	s.sel = 0
}

// fuzzyScore reports whether query matches cand as a case-insensitive
// subsequence, scoring consecutive matches and word-start matches up and
// penalizing gaps -- enough ranking for a handful of lanes, pipelines, and
// runs; no external matcher dependency. The greedy scan runs once from every
// position the query could start at (so "or" anchors on "_orders", not the
// "o" of "load") and the best alignment wins; candidates are tiny, the
// quadratic worst case is irrelevant.
func fuzzyScore(query, cand string) (int, bool) {
	if query == "" {
		return 0, true
	}
	q := []rune(strings.ToLower(query))
	c := []rune(strings.ToLower(cand))
	best, matched := 0, false
	for start := 0; start <= len(c)-len(q); start++ {
		if c[start] != q[0] {
			continue
		}
		if score, ok := greedyScore(q, c, start); ok && (!matched || score > best) {
			best, matched = score, true
		}
	}
	return best, matched
}

// greedyScore scans cand left to right from start, consuming query runes in
// order, and scores the alignment.
func greedyScore(q, c []rune, start int) (int, bool) {
	score, qi, last := 0, 0, -2
	for ci := start; ci < len(c) && qi < len(q); ci++ {
		if c[ci] != q[qi] {
			continue
		}
		switch {
		case ci == last+1:
			score += 2 // consecutive run
		case ci == 0 || isWordBreak(c[ci-1]):
			score += 3 // word start
		default:
			score++
		}
		if last >= 0 {
			score -= (ci - last - 1) / 4 // mild gap penalty
		}
		last = ci
		qi++
	}
	if qi < len(q) {
		return 0, false
	}
	return score, true
}

// isWordBreak reports a rune that starts a new word for scoring purposes.
func isWordBreak(r rune) bool {
	return r == '_' || r == '-' || r == '.' || r == '/' || unicode.IsSpace(r)
}
