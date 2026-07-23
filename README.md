<p align="center">
  <img src="docs/assets/iris-goddess.png" alt="Iris, goddess of the rainbow, pouring water from a jug" width="100%">
</p>

---

<p align="center">
  <strong>Iris Lakehouse</strong> an engine for data lineage
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25%2B-00ADD8?style=for-the-badge&logo=go" alt="Go 1.25+">
  <img src="https://img.shields.io/badge/Postgres-16%2B-4169E1?style=for-the-badge&logo=postgresql&logoColor=white" alt="Postgres 16+">
  <img src="https://img.shields.io/badge/cgo-free-brightgreen?style=for-the-badge" alt="cgo-free">
</p>

----

**Iris Lakehouse** is a data engine bundled with a CLI tool, developed in Go. The engine is built in the image of a glorified cron, reimagined as a go-based daemon that orchestrates routine tasks.<br/>Here those tasks are called pipelines, written in any language, and their state and produced data are stored in an extendable Postgres cluster.

On top of that, every row's lineage is recorded and treated as a first-class feature. Each write is attributed in-transaction to the run, binary, and declaration that produced it, then sealed into an ed25519-signed, tamper-evident journal. `iris data provenance <table> <pk>` answers where any row came from, at any point in its history.<br/>There are no managed services to wire up and no external dependencies. Made to run on machines of any size, scaling from a single server to a multi-instance, high-availability deployment.

---

## Quick install

One command, no dependencies. Installs the latest prebuilt static binary.

```sh
curl -fsSL https://install.iris-lakehouse.bymarreco.com | bash
```

On Windows, in PowerShell:

```powershell
irm https://install.iris-lakehouse.bymarreco.com/install.ps1 | iex
```

The installer ends by printing the first commands of a real session — see [Getting started](#getting-started).

### Snapshot channel (bleeding edge)

Want the newest code before it ships? Every merge to `development` automatically publishes a rolling [`snapshot` prerelease](https://github.com/MateusAMP2119/iris-lakehouse/releases/tag/snapshot) — same static binaries, same installer, no tests gate it, so it lands minutes after the merge:

```sh
curl -fsSL https://install.iris-lakehouse.bymarreco.com/snapshot | bash
```

```powershell
irm https://install.iris-lakehouse.bymarreco.com/snapshot.ps1 | iex   # Windows
```

Already have iris installed? Switch channels in place — no installer needed:

```sh
iris update --snapshot   # hop onto the rolling development build
iris update              # hop back to the latest stable release
```

`iris --version` reports a snapshot build as `v<next>-snapshot.<date>.<commit>`, so you always know exactly what you're running. The stable command above never picks up snapshots — GitHub's `latest` release excludes prereleases. To go back to stable, just re-run the normal install command.

Have Go 1.25+? `go install` works too:

```sh
go install github.com/MateusAMP2119/iris-lakehouse/cmd/iris@latest
```

Or build from source:

```sh
git clone https://github.com/MateusAMP2119/iris-lakehouse.git
cd iris-lakehouse
go build -o iris ./cmd/iris    # cgo-free static binary
```

Then bootstrap the engine. Iris provisions and manages its own Postgres (or point it at an external Postgres 16+ cluster via `--pg-dsn`):

```sh
iris engine install       # provision managed Postgres + meta schema
iris engine start -d      # start the daemon: leader election, lanes, read API
iris ps                   # confirm it's alive: live view of role, uptime, runs, host load (q quits)
```

Updating later is one command; it fetches the latest release, verifies its checksum, and swaps the binary in place:

```sh
iris update
```

Leaving? Iris removes itself (prompts first; refuses while a daemon runs until you `iris engine stop && iris engine uninstall`):

```sh
iris uninstall
```

Binary broken? Remove it by hand — `rm ~/.iris/bin/iris` — then reinstall or delete `~/.iris` entirely.

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
| `iris engine` | `start`, `stop`, `install`, `uninstall`, `logs`, `inspect`, `connect`, `service …` | Daemon and host lifecycle |
| `iris ps` | *(root verb)* | Engine process status: a live full-screen view on a terminal (lanes, pipelines, runs, log tails); the `GET /ps` JSON envelope when piped or under `--json` (`--all` widens the JSON runs to history) |
| `iris update` | *(root verb)* | Self-replace the installed binary with the latest release (`--snapshot` for the rolling build) |
| `iris uninstall` | *(root verb)* | Remove the installed iris binary itself (prompts first; engine must be down) |

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

## Tested

The suite is database-free: unit tests for pure logic and integration tests that use fakes and local process I/O — including the architecture tests that fail the build when a dependency points the wrong way.

```sh
go test ./...   # unit + integration (database-free)
```

There is no test CI: the suite and golangci-lint run locally before every merge. Nothing merges red.

---

## Documentation

| Document | What's covered |
|---|---|
| [Epics](docs/Iris%20Epics.md) | The 15 capability epics (E00–E14) and their build-dependency order |
| [CLAUDE.md](CLAUDE.md) | Build and test commands, branching rules, and conventions |

---

## Releasing

Merging a PR into `master` is now the **only** action required to produce a new release of the CLI.

- The release workflow automatically builds cross-platform binaries and publishes a GitHub release.
- Version bumps default to **patch** (e.g. `v0.5.0` → `v0.5.1`).
- Override the bump by adding one of these labels to the PR **before merging**:
  - `release:major` (or `major` / `breaking`)
  - `release:minor` (or `minor` / `feature`)
- The recommended one-liner always delivers the latest release:

  ```sh
  curl -fsSL https://install.iris-lakehouse.bymarreco.com | bash
  ```

---

## Status

All 15 epics (E00–E14) are **complete on `development`**: full suite green. Epic checkpoint merges to `master` are in progress.

Built test-first, end to end, by AI coding agents working under the conventions in [CLAUDE.md](CLAUDE.md): every line of source written against failing tests, nothing merged red.
