<p align="center">
  <img src="docs/assets/iris-goddess.png" alt="Iris, goddess of the rainbow, pouring water from a jug" width="100%">
</p>

# Iris 🌈

<p align="center">
  <strong>Provenance-first data engine and pipeline orchestrator — git blame for every database row.</strong>
</p>

<p align="center">
  <a href="https://github.com/MateusAMP2119/iris-engine-cli/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/MateusAMP2119/iris-engine-cli/ci.yml?branch=development&style=for-the-badge&label=CI" alt="CI"></a>
  <img src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?style=for-the-badge&logo=go" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/Postgres-16%2B-4169E1?style=for-the-badge&logo=postgresql&logoColor=white" alt="Postgres 16+">
  <img src="https://img.shields.io/badge/cgo-free-brightgreen?style=for-the-badge" alt="cgo-free">
  <img src="https://img.shields.io/badge/Contracts-517%20traced-blueviolet?style=for-the-badge" alt="517 contracts">
</p>

**Iris is a single Go binary that runs your data pipelines and remembers where every row came from.** Every write is attributed **in-transaction** to its exact run, binary, and declaration — so `iris data provenance` can answer any row's origin, forever, backed by an ed25519-signed, tamper-evident journal. Think *Docker Compose for routines*: a pipeline is a folder with one script (any language) and one `iris-declare.yaml`. No cron, no retries, no YAML programming language — a deliberately small model with hard guarantees.

<table>
<tr><td><b>Provenance is core, not a plugin</b></td><td>Statement-level triggers journal every write into <code>data_journal</code> in the same transaction as the write itself. No opt-out, no sidecar, no eventual consistency. One journal feeds two consumers: row-level provenance and undo.</td></tr>
<tr><td><b>Tamper-evident history</b></td><td>Journal partitions are sealed, compacted, and archived into a content-addressed object store, chained together with ed25519-signed checkpoints. History can be verified, not just trusted.</td></tr>
<tr><td><b>Authors never touch credentials</b></td><td>The engine owns least-privilege Postgres roles and injects connections into pipeline processes. <code>reads</code>/<code>writes</code> in the declaration are access control, enforced at the database — not documentation.</td></tr>
<tr><td><b>No clock, anywhere</b></td><td>Orchestration is purely reactive: <code>depends_on</code> gates eligibility, lane <code>order</code> is the sole sequence, perpetual lanes loop. No cron, no schedules, no timeouts, no retries-with-backoff. A run ends by exiting or by <code>iris run cancel</code>.</td></tr>
<tr><td><b>Failures are a worklist</b></td><td>A failed run parks in the dead-letter queue with its full context. Triage with <code>iris dl list</code>, replay on demand, drain when resolved. Failure propagates through <code>depends_on</code> — downstream never runs on missing input.</td></tr>
<tr><td><b>Reproducible by construction</b></td><td>Two artifact modes (dev source / built binary) × two data modes (disposable / permanent). Permanent data requires a built, content-addressed binary, and every run is snapshot-pinned.</td></tr>
<tr><td><b>Any language, no shell</b></td><td>Pipelines are direct-exec engine-owned subprocesses — Python, R, Bash, a compiled binary, anything with a shebang or an entry point. No DSL to learn, no shell-injection surface.</td></tr>
<tr><td><b>HA built in</b></td><td>Any number of daemon candidates, exactly one leader via Postgres advisory lock. Standbys serve reads and redirect mutations to the leader with explicit guidance.</td></tr>
<tr><td><b>A real read API</b></td><td>One GET-only HTTP server (unix socket, optional TCP+TLS) guarded by scoped PATs. Raw tables at <code>/data/{schema}/{table}</code>, declared data products at <code>/q/{endpoint}</code>, NDJSON streaming, keyset pagination.</td></tr>
</table>

---

## Install

```sh
git clone https://github.com/MateusAMP2119/iris-engine-cli.git
cd iris-engine-cli
go build -o iris .        # cgo-free static binary — no toolchain beyond Go 1.25+
```

Iris manages its own Postgres (embedded) or points at an external cluster (Postgres 16+) via `--pg-dsn`.

```sh
iris engine install       # provision managed Postgres + meta schema
iris engine start -d      # start the daemon: leader election, lanes, read API
iris engine info          # confirm it's alive
```

---

## Getting started

A pipeline is a folder — one script, one declaration:

```
my-pipeline/
├── iris-declare.yaml
└── ingest.py             # any language, direct-exec — no shell
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

No `schedule`, no `retries`, no `timeout`, no `params` — those fields don't exist by design. Tables are declared in `schemas/` and evolved by declarative, additive-only diff. Then:

```sh
iris declare apply ./my-pipeline    # register pipeline + schemas
iris pipeline run ingest-orders     # queue a run
iris run list --graph               # live DAG of runs
iris run show <run> --trace         # what a run read, wrote, depended on
iris run logs <run>                 # captured stdout/stderr

# the headline act: where did this row come from?
iris data provenance core.orders 42
```

That last command answers with the exact run, binary, and declaration that produced row `42` — attributed at write time, in the same transaction, verifiable against the signed checkpoint chain.

---

## CLI quick reference

Global flags everywhere: `--json` (machine output), `--socket`, `--host`, `--token`.

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

Exit codes are a contract, not an accident:

| Code | Meaning |
|---|---|
| `0` | success |
| `2` | usage error |
| `3` | no daemon reachable |
| `4` | operation failed |
| `5` | run dead-lettered |
| `6` | not leader — mutation redirected to the leader's address |

---

## The model in five sentences

1. **A pipeline is a folder** with one `iris-declare.yaml` (exactly 8 fields) and one script, executed directly — never through a shell — as an engine-owned subprocess.
2. **Everything lives in one Postgres cluster, two databases**: `meta` (control state, leader-written, 20 tables) and the data database (your tables plus `public.data_journal`).
3. **Write capture is always on**: triggers journal every write in the same transaction; the journal is partitioned, sealed, and archived with an ed25519-signed checkpoint chain.
4. **Orchestration has no clock**: `depends_on` gates eligibility and propagates failure, lane `order` is the only sequence, lanes are serial within and parallel across.
5. **Run states are `queued → running → succeeded | dead_lettered`** — one non-success terminal state, and the dead-letter worklist is the triage surface.

---

## Read API

One HTTP/1.1 JSON server, GET-only, guarded by PATs. One PAT type gates every network-reachable surface; scopes are any non-empty subset of `{control, read, data}`.

- **Engine state** (scope `read`) — runs, pipelines, lanes, dead letters.
- **Data** (scope `data`) — raw tables at `/data/{schema}/{table}` and declared data products at `/q/{endpoint}`. Bulk reads stream NDJSON (`Accept: application/x-ndjson`); pagination is keyset-only (`after`/`before`), never offset. Data PATs map to engine-managed read-only `NOLOGIN` Postgres roles assumed via `SET ROLE` — the API can't read more than the token's role allows.

```sh
iris pat create --scope read --label ci-dashboard   # token printed exactly once
curl -H "Authorization: Bearer $TOKEN" http://host:port/q/daily-orders
```

---

## Architecture

```
cli ──► daemon/api ──► dispatch ──► store (meta db) / pg (data db) / exec
                          │
                       archive (object store, sealed partitions)
        declare · build · pat  (leaf packages)
```

- **One module, one main** — everything under `internal/`, only `cmd/iris` builds.
- **Import graph flows one direction** and is enforced by tests (`internal/arch`).
- **Dependencies are deliberately few**: `pgx`, `cobra`, `goccy/go-yaml`, `argon2id`, `embedded-postgres`. No ORM, no migration framework, no scheduler library, no SQLite, no cgo.
- **Cross-compiles clean**: `CGO_ENABLED=0` across linux/darwin × amd64/arm64 in CI.

---

## Spec-first, test-driven

This repo is built spec-first: [`docs/Iris Specification Inventory.md`](docs/Iris%20Specification%20Inventory.md) is the source of truth and the test suite is its executable form. Every behavior is a numbered contract in [`spec/contracts.yaml`](spec/contracts.yaml) — **517 contracts** — and a traceability gate fails the build if any non-exempt contract lacks a claiming test. The implementation is regenerable; the spec and the suite are the durable assets.

| Tier | Contracts | What it means |
|---|---|---|
| unit | 191 | pure logic, no I/O |
| integration | 229 | fakes and local process I/O, no live Postgres |
| conformance | 76 | the real shipped binary, a running daemon, real Postgres |
| exempt | 21 | naming/rationale/doctrine — no test required |

```sh
go test -race ./...                                                        # unit + integration (database-free)
go test ./internal/trace/...                                               # traceability gate
go test -race -tags conformance -timeout 20m ./internal/conformance/...   # real binary + real Postgres, ~11 min
```

CI runs all of the above on Go 1.25 and 1.26, plus golangci-lint and the cross-compile matrix, with conformance against Postgres 17. Nothing merges red.

---

## Documentation

| Document | What's covered |
|---|---|
| [Specification Inventory](docs/Iris%20Specification%20Inventory.md) | The full spec — every behavior, table, endpoint, and doctrine. Source of truth; on conflict, the spec wins |
| [Epics](docs/Iris%20Epics.md) | The 15 capability epics (E00–E14) and their build-dependency order |
| [Tasks](docs/Tasks) | Per-task briefs: contract lists, dependencies, Done-when checklists |
| [BUILD_STATE.md](BUILD_STATE.md) | Live build status — every task, PR, and open item |
| [CLAUDE.md](CLAUDE.md) | TDD doctrine, branching rules, and conventions |

---

## Status

All 15 epics (E00–E14) are **complete on `development`**: full CI green, zero unclaimed contracts, full conformance suite passing under `-race`. Epic checkpoint merges to `master` are in progress.

Built spec-first and test-first, end to end, by AI coding agents working under the TDD doctrine in [CLAUDE.md](CLAUDE.md) — every line of source written by a coder agent against failing contract tests, every merge gated by the traceability suite.
