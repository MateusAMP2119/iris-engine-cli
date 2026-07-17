package conformance

// This file holds the shared turn-protocol pipeline scripts the conformance
// suite runs (#206): resident workers that speak the JSON Lines protocol --
// stdin carries go/row/run frames from the engine, stdout answers row frames
// and one done/error terminal echoing the turn number, stderr stays log. The
// engine mediates every database access, so none of these scripts opens a
// database connection or reads IRIS_DB_URL.

// ShTurnDoneLoop is a POSIX sh resident that answers every turn with a bare
// done: the quiet pipeline. It produces no rows, so under the turn protocol its
// turns record nothing and its lane parks between causes.
const ShTurnDoneLoop = `while read line; do
  case "$line" in
  *'"go"'*) turn=$(printf '%s' "$line" | sed 's/.*"turn"://;s/[^0-9].*//') ;;
  *'"run"'*) printf '{"event":"done","turn":%s}\n' "$turn" ;;
  esac
done
`

// PyTurnPrelude is the Python frame-loop prelude the per-test scripts build on:
// turn_loop reads engine frames and calls on_turn(turn, rows) at each run
// frame, where rows are the fed input row frames; emit writes one output row
// frame and done/error write the terminal. Every write flushes (the engine
// reads line by line).
const PyTurnPrelude = `import json, sys

def emit(table, row):
    sys.stdout.write(json.dumps({"event": "row", "table": table, "row": row}) + "\n")
    sys.stdout.flush()

def done(turn):
    sys.stdout.write(json.dumps({"event": "done", "turn": turn}) + "\n")
    sys.stdout.flush()

def error(turn, reason, detail=None):
    f = {"event": "error", "turn": turn, "reason": reason}
    if detail is not None:
        f["detail"] = detail
    sys.stdout.write(json.dumps(f) + "\n")
    sys.stdout.flush()

def turn_loop(on_turn):
    turn, rows = None, []
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        f = json.loads(line)
        if f["event"] == "go":
            turn, rows = f["turn"], []
        elif f["event"] == "row":
            rows.append(f)
        elif f["event"] == "run":
            on_turn(turn, rows)
`
