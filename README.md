<img src="docs/assets/iris-goddess.png" alt="Iris, goddess of the rainbow, pouring water from a jug" width="100%">

<p align="center">
  <strong>Provenance-first data engine and pipeline orchestrator — git blame for every database row.</strong>
</p>

<p align="center">
  <a href="https://github.com/MateusAMP2119/iris-engine-cli/actions/workflows/ci.yml"><img src="https://github.com/MateusAMP2119/iris-engine-cli/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/go-1.25%2B-00ADD8?logo=go" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/postgres-16%2B-4169E1?logo=postgresql&logoColor=white" alt="Postgres 16+">
  <img src="https://img.shields.io/badge/cgo-free-brightgreen" alt="cgo-free">
</p>

---

Iris is a single Go binary (`iris`) that runs your data pipelines and remembers where every row came from. Every write is attributed **in-transaction** to its exact run, binary, and declaration — so `iris data provenance` can answer any row's origin, forever. Think *Docker Compose for routines*: a pipeline is a folder with one script (any language) and one `iris-declare.yaml`.

## Why Iris

- **Provenance is core, not a plugin.** Statement-level triggers journal every write into `data_journal` in the same transaction as the write itself. No opt-out, no sidecar, no eventual consistency. One journal feeds two consumers: row-level provenance and undo.
- **Tamper-evident history.** Journal partitions are sealed, compacted, and archived into a content-addressed object store, chained together with ed25519-signed checkpoints.
- **Authors never touch credentials.** The engine owns least-privilege Postgres roles and injects connections into pipeline processes. `reads`/`writes` in the declaration are access control, enforced at the database.
- **No clock, anywhere.** Orchestration is purely reactive: `depends_on` gates eligibility, lane `order` is the sole sequence, perpetual lanes loop. No cron, no schedules, no timeouts, no retries-with-backoff. A run ends by exiting or by `iris run cancel`; failures park in a dead-letter worklist and replay on demand.
- **Reproducible by construction.** Two artifact modes (dev source / built binary) × two data modes (disposable / permanent). Permanent data requires a built, content-addressed binary, and every run is snapshot-pinned.
- **HA built in.** Any number of daemon candidates, exactly one leader via Postgres advisory lock. Standbys serve reads and redirect mutations to the leader.

## A pipeline is a folder

```
my-pipeline/
├── iris-declare.yaml
└── ingest.py          # any language, direct-exec — no shell
```

```yaml
# iris-declare.yaml — exactly these fields exist, nothing else
name: ingest-orders
run: ./ingest.py
lane: nightly
reads: [staging.raw_orders]
writes: [core.orders]
depends_on: [fetch-orders]
env:
  MODE: full
env_file: .env
```

No `schedule`, no `retries`, no `timeout`, no `params` — those fields don't exist by design. Tables are declared in `schemas/` and evolved by declarative, additive-only diff.

## Quickstart

```sh
go build -o iris .            # cgo-free static binary

iris engine install           # provision managed Postgres + meta schema
iris engine start -d          # start the daemon (leader election, lanes)

iris declare apply ./my-pipeline
iris pipeline run ingest-orders

iris run list --graph         # live DAG of runs
iris run show <run> --trace   # what a run read, wrote, depended on

# the headline act: where did this row come from?
iris data provenance core.orders 42
```

## CLI at a glance

Global flags everywhere: `--json`, `--socket`, `--host`, `--token`.

| Noun | Verbs | Purpose |
|---|---|---|
| `iris declare` | `apply`, `destroy` | Apply or remove pipeline/schema declarations |
| `iris pipeline` | `build`, `promote`, `run`, `list`, `show` | Artifact lifecycle and manual runs |
| `iris run` | `list`, `show`, `logs`, `cancel` | Inspect and control runs (`--graph`, `--trace`) |
| `iris data` | `provenance` | Row-level origin lookup |
| `iris workload` | `show`, `wipe` | Disposable-data workload management |
| `iris deadletter` (`dl`) | `list`, `show`, `replay`, `drain` | Failure triage worklist |
| `iris endpoint` | `apply`, `remove`, `list`, `show` | Declared read endpoints at `GET /q/{endpoint}` |
| `iris pat` | `create`, `list`, `revoke` | Personal access tokens (scopes: `control`, `read`, `data`) |
| `iris engine` | `start`, `stop`, `install`, `uninstall`, `info`, `logs`, `inspect`, `stats`, `service …` | Daemon and host lifecycle |

Exit codes are a contract: `0` success · `2` usage · `3` no daemon · `4` operation failed · `5` dead-lettered · `6` not leader.

## Read API

One HTTP/1.1 JSON server (unix socket, optional TCP+TLS), GET-only, guarded by PATs:

- **Engine state** (scope `read`) — runs, pipelines, lanes, dead letters.
- **Data** (scope `data`) — raw tables at `/data/{schema}/{table}` and declared data products at `/q/{endpoint}`. Bulk reads stream NDJSON; pagination is keyset-only (`after`/`before`). Data PATs map to engine-managed read-only Postgres roles assumed via `SET ROLE`.

## Architecture

Everything lives in one Postgres cluster, two databases: `meta` (control state, leader-written, 20 tables) and the data database (your tables + `public.data_journal`).

```
cli ──► daemon/api ──► dispatch ──► store (meta db) / pg (data db) / exec
                          │
                       archive (object store, sealed partitions)
        declare · build · pat  (leaf packages)
```

The import graph flows one direction only, and it's enforced by tests (`internal/arch`). Dependencies are deliberately few: `pgx`, `cobra`, `goccy/go-yaml`, `argon2id`, `embedded-postgres`. No ORM, no migration framework, no scheduler library, no cgo.

## Spec-first, test-driven

This repo is built spec-first: `docs/Iris Specification Inventory.md` is the source of truth, and the test suite is its executable form. Every behavior is a numbered contract in [`spec/contracts.yaml`](spec/contracts.yaml) — **517 contracts** across three tiers — and a traceability gate fails the build if any non-exempt contract lacks a claiming test.

| Tier | Contracts | What it means |
|---|---|---|
| unit | 191 | pure logic, no I/O |
| integration | 229 | fakes and local process I/O, no live Postgres |
| conformance | 76 | the real shipped binary, a running daemon, real Postgres |
| exempt | 21 | naming/rationale/doctrine — no test required |

```sh
# unit + integration (database-free)
go test -race ./...

# traceability gate
go test ./internal/trace/...

# conformance (real binary, real Postgres 16+, ~11 min)
go test -race -tags conformance -timeout 20m ./internal/conformance/...
```

CI runs all of the above on Go 1.25 and 1.26, plus golangci-lint and a cgo-free cross-compile matrix (linux/darwin × amd64/arm64), with conformance against Postgres 17.

## Documentation

- [`docs/Iris Specification Inventory.md`](docs/Iris%20Specification%20Inventory.md) — the specification (source of truth)
- [`docs/Iris Epics.md`](docs/Iris%20Epics.md) — the 15 epics and build order
- [`docs/Tasks/`](docs/Tasks) — per-task briefs with contract lists
- [`BUILD_STATE.md`](BUILD_STATE.md) — live build status
- [`CLAUDE.md`](CLAUDE.md) — TDD doctrine and branching rules

## Status

All 15 epics (E00–E14) are complete on `development`: full CI green, zero unclaimed contracts, full conformance suite passing under `-race`. Epic checkpoint merges to `master` are in progress.
