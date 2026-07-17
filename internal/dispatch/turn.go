package dispatch

import (
	"encoding/json"
	"fmt"
)

// This file is the turn protocol model (#206): the pure frame codec and per-turn
// collection state machine for resident pipelines. A turn is one engine-fed
// iteration over the JSON Lines protocol -- stdin carries the engine half
// (go, input rows, run), stdout the pipeline half (output rows, then exactly one
// terminal done or error frame echoing the turn number), stderr stays free-form
// log. The model here is pure: it renders engine frames, parses pipeline frames,
// and enforces the protocol's frame discipline (rows inside the declared writes,
// one terminal frame, correct turn echo) with no I/O; the daemon's resident
// session owns the pipes and drives this model line by line.

// The turn protocol frame events. Engine to pipeline: go (turn header), row
// (input row), run (input complete). Pipeline to engine: row (output row), done
// (turn succeeded), error (turn failed, declared by the pipeline).
const (
	// TurnEventGo opens a turn: {"event":"go","turn":N}.
	TurnEventGo = "go"
	// TurnEventRow carries one row either direction: {"event":"row","table":"s.t","row":{...}}.
	TurnEventRow = "row"
	// TurnEventRun closes the engine's input feed: {"event":"run"}.
	TurnEventRun = "run"
	// TurnEventDone is the pipeline's success terminal: {"event":"done","turn":N}.
	TurnEventDone = "done"
	// TurnEventError is the pipeline's declared-failure terminal: {"event":"error","turn":N,"reason":"...","detail":{}}.
	TurnEventError = "error"
)

// turnFrame is the one wire shape both protocol halves share; a field absent from
// a given event marshals away under omitempty.
type turnFrame struct {
	Event  string          `json:"event"`
	Turn   *int64          `json:"turn,omitempty"`
	Table  string          `json:"table,omitempty"`
	Row    json.RawMessage `json:"row,omitempty"`
	Reason string          `json:"reason,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
}

// EncodeGoFrame renders the turn-opening header the engine writes first.
func EncodeGoFrame(turn int64) string {
	return `{"event":"go","turn":` + fmt.Sprintf("%d", turn) + `}`
}

// EncodeRunFrame renders the input-complete frame the engine writes after the
// input rows (zero rows is a normal turn).
func EncodeRunFrame() string {
	return `{"event":"run"}`
}

// EncodeRowFrame renders one row frame. table is the dotted schema.table; row
// must be a JSON object (the feed produces row_to_json output). The row bytes
// ride verbatim.
func EncodeRowFrame(table string, row json.RawMessage) (string, error) {
	b, err := json.Marshal(turnFrame{Event: TurnEventRow, Table: table, Row: row})
	if err != nil {
		return "", fmt.Errorf("dispatch: encode row frame for %s: %w", table, err)
	}
	return string(b), nil
}

// FrameError is a turn protocol violation: an offending stdout line and why it
// violates the protocol. The daemon dead-letters the turn with the line quoted
// verbatim in the dead letter's detail, so the operator sees exactly what the
// pipeline said.
type FrameError struct {
	// Line is the offending stdout line, verbatim.
	Line string
	// Cause names the violation (unparseable, undeclared table/field, wrong echo, ...).
	Cause string
}

// Error renders the violation with the offending line quoted.
func (e *FrameError) Error() string {
	return fmt.Sprintf("turn protocol violation: %s: %q", e.Cause, e.Line)
}

// WriteSet is a pipeline's declared writes: dotted schema.table to the set of
// declared fields. It is the enforcement surface for output rows -- the engine
// performs every write, so a row outside it is refused as a protocol violation,
// never written.
type WriteSet map[string]map[string]bool

// checkRow validates one output row against the declared writes: the table must
// be declared, the row must be a non-empty JSON object, and every key must be a
// declared field. It returns the violation cause, or "" when the row is admissible.
func (ws WriteSet) checkRow(table string, row json.RawMessage) string {
	fields, ok := ws[table]
	if !ok {
		return fmt.Sprintf("table %q is not in the declared writes", table)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(row, &m); err != nil || m == nil {
		return "row is not a JSON object"
	}
	if len(m) == 0 {
		return "row object is empty"
	}
	for k := range m {
		if !fields[k] {
			return fmt.Sprintf("field %q of table %q is not in the declared writes", k, table)
		}
	}
	return ""
}

// TurnRow is one collected output row: the dotted table it targets and the row
// object verbatim, as the pipeline framed it.
type TurnRow struct {
	// Table is the dotted schema.table the row targets.
	Table string
	// Row is the row's JSON object, verbatim.
	Row json.RawMessage
}

// TurnEnd is a turn's terminal frame as collected: a done, or a pipeline-declared
// error carrying its reason and opaque detail.
type TurnEnd struct {
	// Errored reports a declared error terminal (true) versus done (false).
	Errored bool
	// Reason is the pipeline's declared failure reason (error terminal only).
	Reason string
	// Detail is the pipeline's opaque error detail, verbatim (error terminal only).
	Detail json.RawMessage
}

// TurnCollector consumes one turn's stdout lines and enforces the pipeline half
// of the protocol: output rows must fall inside the declared writes, exactly one
// terminal frame ends the turn, and the terminal must echo the turn number. It
// is pure per-turn state; the daemon builds one per turn and feeds it every
// protocol line the session's scanner yields until it reports terminal.
type TurnCollector struct {
	turn   int64
	writes WriteSet
	rows   []TurnRow
	ended  bool
}

// NewTurnCollector builds the collector for one turn over the pipeline's
// declared writes.
func NewTurnCollector(turn int64, writes WriteSet) *TurnCollector {
	return &TurnCollector{turn: turn, writes: writes}
}

// Rows returns the output rows collected so far, in arrival order.
func (c *TurnCollector) Rows() []TurnRow {
	return c.rows
}

// Feed consumes one stdout line. It returns the terminal frame and true once the
// turn ends; a non-nil error is a *FrameError protocol violation (unparseable
// line, row outside the declared writes, a frame after the terminal, or a wrong
// turn echo) that dead-letters the turn with the line quoted.
func (c *TurnCollector) Feed(line string) (TurnEnd, bool, error) {
	if c.ended {
		return TurnEnd{}, false, &FrameError{Line: line, Cause: "frame after the terminal frame"}
	}
	var f turnFrame
	if err := json.Unmarshal([]byte(line), &f); err != nil {
		return TurnEnd{}, false, &FrameError{Line: line, Cause: "unparseable frame"}
	}
	switch f.Event {
	case TurnEventRow:
		if f.Table == "" {
			return TurnEnd{}, false, &FrameError{Line: line, Cause: "row frame has no table"}
		}
		if cause := c.writes.checkRow(f.Table, f.Row); cause != "" {
			return TurnEnd{}, false, &FrameError{Line: line, Cause: cause}
		}
		c.rows = append(c.rows, TurnRow{Table: f.Table, Row: f.Row})
		return TurnEnd{}, false, nil
	case TurnEventDone, TurnEventError:
		if f.Turn == nil || *f.Turn != c.turn {
			return TurnEnd{}, false, &FrameError{Line: line, Cause: fmt.Sprintf("terminal frame does not echo turn %d", c.turn)}
		}
		c.ended = true
		if f.Event == TurnEventError {
			reason := f.Reason
			if reason == "" {
				reason = "pipeline declared an error"
			}
			return TurnEnd{Errored: true, Reason: reason, Detail: f.Detail}, true, nil
		}
		return TurnEnd{}, true, nil
	default:
		return TurnEnd{}, false, &FrameError{Line: line, Cause: fmt.Sprintf("unknown frame event %q", f.Event)}
	}
}
