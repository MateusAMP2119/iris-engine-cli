# Turn protocol

A pipeline is a resident worker process. The engine speaks JSON Lines to it: one JSON object per line, stdin carries the engine's half, stdout carries the pipeline's half, stderr is free-form log (captured into the run log when a turn records). The worker holds no database credentials; the engine performs every read and write.

## Engine to pipeline (stdin)

```json
{"event":"go","turn":841}
{"event":"row","table":"raw.orders","row":{"id":9,"total":40}}
{"event":"run"}
```

- `go` opens the turn. `turn` is a per-session counter; echo it in the terminal frame.
- `row` frames are the input feed: every data-journal entry for the pipeline's declared `reads` since its last consumed position, deduplicated to each row key's newest entry, carrying the row's CURRENT values projected to the declared fields. Zero rows is a normal turn.
- `run` closes the input. Answer after it.

## Pipeline to engine (stdout)

```json
{"event":"row","table":"marts.daily","row":{"day":"2026-07-17","sum":52}}
{"event":"done","turn":841}
```

or, declaring failure:

```json
{"event":"error","turn":841,"reason":"upstream gone","detail":{"code":3}}
```

- `row` frames are output. Each `table` must be in the declared `writes` and every key of `row` a declared field. The engine upserts on the table's primary key (`INSERT ... ON CONFLICT pk DO UPDATE`); absent columns stay untouched. Values cast from JSON server-side.
- Exactly one terminal frame ends the turn: `done` or `error`, echoing the turn number. `reason` and `detail` land verbatim in the dead letter.
- Exiting after the terminal frame is fine (one-shot worker); staying resident skips respawn cost on the next turn.

## What a turn commits

One atomic data transaction per turn: output rows, their capture-journal stamps (attributed to the run id), and the advanced feed position. Any failure commits nothing.

- done with output rows: a run row records (running, then succeeded with exit code, LSN, journal window, log ref).
- done with no rows and no new input: nothing is written anywhere. The lane parks until the next engine cause. `iris ps` counts these turns per resident (`turns_since_run`, the `t+N` badge).
- done with input but no rows: only the feed position advances; no run row.
- error frame, protocol violation, or worker death mid-turn: the run records directly dead-lettered, and the no-retry brake holds until a manual run, replay, or drain.

## Protocol violations

Dead-letter the turn with the offending line quoted: a non-JSON stdout line, an unknown event, a row outside the declared writes, a wrong turn echo, a frame after the terminal. The worker is recycled afterward.

## Operator surface

A hung turn holds its lane and leaves no run row; `iris pipeline stop <name>` parks the pipeline (minting the park row when no live run exists) and kills the worker. `iris pipeline run <name>` releases a park and runs one turn as a fresh one-shot worker.

## Minimal worker (sh)

```sh
while read -r line; do
  case "$line" in
  *'"event":"go"'*) turn=$(printf '%s' "$line" | sed 's/.*"turn"://;s/[^0-9].*//') ;;
  *'"event":"run"'*)
    printf '{"event":"row","table":"testrun.events","row":{"id":1,"note":"hi"}}\n'
    printf '{"event":"done","turn":%s}\n' "$turn"
    ;;
  esac
done
```

Python speakers: see `internal/conformance/turnscripts.go` (`PyTurnPrelude`) and the catalog samples (`iris catalog`).
