package dispatch

import (
	"encoding/json"
	"fmt"
	"strings"
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
// (input row), run (input complete), res (a call's reply). Pipeline to engine:
// row (output row), call (a declared-plugin verb call, #215), done (turn
// succeeded), error (turn failed, declared by the pipeline).
const (
	// TurnEventGo opens a turn: {"event":"go","turn":N}.
	TurnEventGo = "go"
	// TurnEventRow carries one row either direction: {"event":"row","table":"s.t","row":{...}}.
	TurnEventRow = "row"
	// TurnEventRun closes the engine's input feed: {"event":"run"}.
	TurnEventRun = "run"
	// TurnEventCall requests one declared-plugin verb mid-turn:
	// {"event":"call","call":N,"verb":"alias.verb","args":{...}}.
	TurnEventCall = "call"
	// TurnEventRes is the engine's always-delivered reply to a call:
	// {"event":"res","call":N,"ok":true,"result":{...}} or
	// {"event":"res","call":N,"ok":false,"error":"..."}.
	TurnEventRes = "res"
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
	Call   *int64          `json:"call,omitempty"`
	Verb   string          `json:"verb,omitempty"`
	Args   json.RawMessage `json:"args,omitempty"`
	OK     *bool           `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
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

// EncodeResOK renders a call's success reply (echoed id, ok true, result).
func EncodeResOK(call int64, result json.RawMessage) (string, error) {
	ok := true
	b, err := json.Marshal(turnFrame{Event: TurnEventRes, Call: &call, OK: &ok, Result: result})
	if err != nil {
		return "", fmt.Errorf("dispatch: encode res frame for call %d: %w", call, err)
	}
	return string(b), nil
}

// EncodeResErr renders a call's failure reply; the engine always replies, a
// plugin crash or timeout produces this frame, never silence.
func EncodeResErr(call int64, msg string) (string, error) {
	ok := false
	b, err := json.Marshal(turnFrame{Event: TurnEventRes, Call: &call, OK: &ok, Error: msg})
	if err != nil {
		return "", fmt.Errorf("dispatch: encode res frame for call %d: %w", call, err)
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

// CallSet is a pipeline's declared calls (alias → manifest verbs): the call
// frames' enforcement surface, like WriteSet for rows (#215).
type CallSet map[string]map[string]bool

// checkVerb validates "alias.verb" against the declared calls, returning the
// parsed halves or the violation cause.
func (cs CallSet) checkVerb(verb string) (alias, name, cause string) {
	alias, name, ok := strings.Cut(verb, ".")
	if !ok || alias == "" || name == "" {
		return "", "", fmt.Sprintf("call verb %q is not alias.verb", verb)
	}
	verbs, ok := cs[alias]
	if !ok {
		return "", "", fmt.Sprintf("plugin alias %q is not in the declared plugins", alias)
	}
	if !verbs[name] {
		return "", "", fmt.Sprintf("verb %q is not declared by plugin %q", name, alias)
	}
	return alias, name, ""
}

// TurnCall is one collected call request the daemon must service and answer.
type TurnCall struct {
	// Call is the pipeline-chosen call id the res frame echoes.
	Call int64
	// Alias is the declared plugin alias the verb rode.
	Alias string
	// Verb is the manifest verb inside the alias.
	Verb string
	// Args is the call's args JSON object, verbatim (empty means no args).
	Args json.RawMessage
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

// TurnCollector consumes one turn's stdout lines and enforces the pipeline
// half of the protocol: rows inside the declared writes, calls inside the
// declared plugins (one outstanding), one terminal frame echoing the turn.
type TurnCollector struct {
	turn    int64
	writes  WriteSet
	calls   CallSet
	rows    []TurnRow
	pending *int64
	ended   bool
}

// NewTurnCollector builds the collector for one turn (nil calls declares nothing).
func NewTurnCollector(turn int64, writes WriteSet, calls CallSet) *TurnCollector {
	return &TurnCollector{turn: turn, writes: writes, calls: calls}
}

// Rows returns the output rows collected so far, in arrival order.
func (c *TurnCollector) Rows() []TurnRow {
	return c.rows
}

// ReplyDelivered re-admits call frames after the outstanding call's res frame
// went out (MVP: one call at a time; ids on the wire make concurrency an upgrade).
func (c *TurnCollector) ReplyDelivered() {
	c.pending = nil
}

// Feed consumes one stdout line: an admissible call frame returns the call to
// service (then ReplyDelivered), the terminal returns true, and a *FrameError
// is a protocol violation that dead-letters the turn with the line quoted.
func (c *TurnCollector) Feed(line string) (TurnEnd, *TurnCall, bool, error) {
	if c.ended {
		return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: "frame after the terminal frame"}
	}
	var f turnFrame
	if err := json.Unmarshal([]byte(line), &f); err != nil {
		return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: "unparseable frame"}
	}
	switch f.Event {
	case TurnEventRow:
		if f.Table == "" {
			return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: "row frame has no table"}
		}
		if cause := c.writes.checkRow(f.Table, f.Row); cause != "" {
			return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: cause}
		}
		c.rows = append(c.rows, TurnRow{Table: f.Table, Row: f.Row})
		return TurnEnd{}, nil, false, nil
	case TurnEventCall:
		if f.Call == nil {
			return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: "call frame has no call id"}
		}
		if c.pending != nil {
			return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: fmt.Sprintf("call %d before call %d's reply", *f.Call, *c.pending)}
		}
		alias, verb, cause := c.calls.checkVerb(f.Verb)
		if cause != "" {
			return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: cause}
		}
		c.pending = f.Call
		return TurnEnd{}, &TurnCall{Call: *f.Call, Alias: alias, Verb: verb, Args: f.Args}, false, nil
	case TurnEventDone, TurnEventError:
		if f.Turn == nil || *f.Turn != c.turn {
			return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: fmt.Sprintf("terminal frame does not echo turn %d", c.turn)}
		}
		c.ended = true
		if f.Event == TurnEventError {
			reason := f.Reason
			if reason == "" {
				reason = "pipeline declared an error"
			}
			return TurnEnd{Errored: true, Reason: reason, Detail: f.Detail}, nil, true, nil
		}
		return TurnEnd{}, nil, true, nil
	default:
		return TurnEnd{}, nil, false, &FrameError{Line: line, Cause: fmt.Sprintf("unknown frame event %q", f.Event)}
	}
}
