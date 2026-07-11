<p align="center">
  <img src="docs/assets/iris-goddess.png" alt="Iris, goddess of the rainbow, pouring water from a jug" width="100%">
</p>

---

<p align="center">
  <strong>Iris Lakehouse</strong> an engine for data lineage
</p>

<p align="center">
  <a href="https://github.com/MateusAMP2119/iris-engine-cli/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/MateusAMP2119/iris-engine-cli/ci.yml?branch=development&style=for-the-badge&label=CI" alt="CI"></a>
  <img src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?style=for-the-badge&logo=go" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/Postgres-16%2B-4169E1?style=for-the-badge&logo=postgresql&logoColor=white" alt="Postgres 16+">
  <img src="https://img.shields.io/badge/cgo-free-brightgreen?style=for-the-badge" alt="cgo-free">
  <img src="https://img.shields.io/badge/Contracts-517%20traced-blueviolet?style=for-the-badge" alt="517 contracts">
</p>

----

**Iris Lakehouse** is a data engine bundled with a CLI tool, developed in Go. The engine is built in the image of a glorified cron, reimagined as a go-based daemon that orchestrates routine tasks.<br/>Here those tasks are called pipelines, written in any language, and their state and produced data are stored in an extendable Postgres cluster.

On top of that, every row's lineage is recorded and treated as a first-class feature. Each write is attributed in-transaction to the run, binary, and declaration that produced it, then sealed into an ed25519-signed, tamper-evident journal. `iris data provenance <table> <pk>` answers where any row came from, at any point in its history.<br/>There are no managed services to wire up and no external dependencies. Made to run on machines of any size, scaling from a single server to a multi-instance, high-availability deployment.

---

## Quick install

One command, no dependencies. Installs the latest prebuilt static binary:

```sh
curl -fsSL https://raw.githubusercontent.com/MateusAMP2119/iris-engine-cli/master/install.sh | sh
```

Have Go 1.25+? `go install` works too:

```sh
go install github.com/MateusAMP2119/iris-engine-cli/cmd/iris@latest
```

Or build from source:

```sh
git clone https://github.com/MateusAMP2119/iris-engine-cli.git
cd iris-engine-cli
go build -o iris ./cmd/iris    # cgo-free static binary
```

Then bootstrap the engine. Iris provisions and manages its own Postgres (or point it at an external Postgres 16+ cluster via `--pg-dsn`):

```sh
iris engine install       # provision managed Postgres + meta schema
iris engine start -d      # start the daemon: leader election, lanes, read API
iris engine info          # confirm it's alive
```

Leaving? Tear down engine state with `iris engine stop && iris engine uninstall`, then remove the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/MateusAMP2119/iris-engine-cli/master/uninstall.sh | sh
```

---

## Getting started

A pipeline is a folder: one script, one declaration:

```
my-pipeline/
├── iris-declare.yaml
└── ingest.py             # any language, direct-exec, no shell
```

```yaml
# iris-declare.yaml: exactly these fields exist, nothing else
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

No `schedule`, no `retries`, no `timeout`, no `params`: those fields don't exist by design. Tables are declared in `schemas/` and evolved by declarative, additive-only diff. Then:

```sh
iris declare apply ./my-pipeline    # register pipeline + schemas
iris pipeline run ingest-orders     # queue a run
iris run list --graph               # live DAG of runs
iris run show <run> --trace         # what a run read, wrote, depended on
iris run logs <run>                 # captured stdout/stderr

# the headline act: where did this row come from?
iris data provenance core.orders 42
```

That last command answers with the exact run, binary, and declaration that produced row `42`: attributed at write time, in the same transaction, verifiable against the signed checkpoint chain.

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
| `6` | not leader; mutation redirected to the leader's address |

---

## The model in five sentences

1. **A pipeline is a folder** with one `iris-declare.yaml` (exactly 8 fields) and one script, executed directly (never through a shell) as an engine-owned subprocess.
2. **Everything lives in one Postgres cluster, two databases**: `meta` (control state, leader-written, 20 tables) and the data database (your tables plus `public.data_journal`).
3. **Write capture is always on**: triggers journal every write in the same transaction; the journal is partitioned, sealed, and archived with an ed25519-signed checkpoint chain.
4. **Orchestration has no clock**: `depends_on` gates eligibility and propagates failure, lane `order` is the only sequence, lanes are serial within and parallel across.
5. **Run states are `queued → running → succeeded | dead_lettered`**: one non-success terminal state, and the dead-letter worklist is the triage surface.

---

## Read API

One HTTP/1.1 JSON server, GET-only, guarded by PATs. One PAT type gates every network-reachable surface; scopes are any non-empty subset of `{control, read, data}`.

- **Engine state** (scope `read`): runs, pipelines, lanes, dead letters.
- **Data** (scope `data`): raw tables at `/data/{schema}/{table}` and declared data products at `/q/{endpoint}`. Bulk reads stream NDJSON (`Accept: application/x-ndjson`); pagination is keyset-only (`after`/`before`), never offset. Data PATs map to engine-managed read-only `NOLOGIN` Postgres roles assumed via `SET ROLE`; the API can't read more than the token's role allows.

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

- **One module, one main**: everything under `internal/`, only `cmd/iris` builds.
- **Import graph flows one direction** and is enforced by tests (`internal/arch`).
- **Dependencies are deliberately few**: `pgx`, `cobra`, `goccy/go-yaml`, `argon2id`, `embedded-postgres`. No ORM, no migration framework, no scheduler library, no SQLite, no cgo.
- **Cross-compiles clean**: `CGO_ENABLED=0` across linux/darwin × amd64/arm64 in CI.

---

## Spec-first, test-driven

This repo is built spec-first: [`docs/Iris Specification Inventory.md`](docs/Iris%20Specification%20Inventory.md) is the source of truth and the test suite is its executable form. Every behavior is a numbered contract in [`spec/contracts.yaml`](spec/contracts.yaml), **517 contracts** in all, and a traceability gate fails the build if any non-exempt contract lacks a claiming test. The implementation is regenerable; the spec and the suite are the durable assets.

| Tier | Contracts | What it means |
|---|---|---|
| unit | 191 | pure logic, no I/O |
| integration | 229 | fakes and local process I/O, no live Postgres |
| conformance | 76 | the real shipped binary, a running daemon, real Postgres |
| exempt | 21 | naming/rationale/doctrine; no test required |

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
| [Specification Inventory](docs/Iris%20Specification%20Inventory.md) | The full spec: every behavior, table, endpoint, and doctrine. Source of truth; on conflict, the spec wins |
| [Epics](docs/Iris%20Epics.md) | The 15 capability epics (E00–E14) and their build-dependency order |
| [Tasks](docs/Tasks) | Per-task briefs: contract lists, dependencies, Done-when checklists |
| [BUILD_STATE.md](BUILD_STATE.md) | Live build status: every task, PR, and open item |
| [CLAUDE.md](CLAUDE.md) | TDD doctrine, branching rules, and conventions |

---

## Status

All 15 epics (E00–E14) are **complete on `development`**: full CI green, zero unclaimed contracts, full conformance suite passing under `-race`. Epic checkpoint merges to `master` are in progress.

Built spec-first and test-first, end to end, by AI coding agents working under the TDD doctrine in [CLAUDE.md](CLAUDE.md): every line of source written by a coder agent against failing contract tests, every merge gated by the traceability suite.
