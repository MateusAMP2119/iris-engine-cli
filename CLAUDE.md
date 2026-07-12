# Iris — engine + CLI

Provenance-first data engine + pipeline orchestrator, one Go binary (`cmd/iris`).
Source of truth: `docs/Iris Specification Inventory.md` (conflict with any other doc → spec wins). Epics + build order: `docs/Iris Epics.md`. Work items: `docs/Tasks/`. Live build status: `BUILD_STATE.md`.

## TDD doctrine (non-negotiable)

Spec = source of truth. Test suite = spec's executable form. Implementation regenerable; durable assets = spec + suite.

1. Every task starts from contract rows in `spec/contracts.yaml` (one row per contract: stable id `Sxx/slug`, doc anchor, tier, status). Add or confirm rows first.
2. Write failing tests for every non-exempt contract at that contract's tier **before any implementation**. Tiers: `unit` (pure logic, no I/O), `integration` (fakes + local process I/O, no live Postgres), `conformance` (real binary, running daemon, real Postgres).
3. Implement to green. Never weaken test to pass; test expectation changes only with spec delta.
4. Tests claim contracts via Go subtest path or `// spec: <contract-id>` annotation.
5. Every commit message names contract ids it satisfies.
6. Traceability gate must pass: every non-exempt manifest contract claimed by test, every test claims real contract. Exempt contracts (naming, rationale, doctrine) live in manifest marked `exempt`, need no test.
7. Nothing merges red: full suite + traceability gate green before merge.

## Commands

- Build: `go build ./...`; binary: `go build -o iris ./cmd/iris` (always cgo-free; release/cross-compile with `CGO_ENABLED=0`).
- Unit + integration (database-free, what CI runs per Go version): `go test -race ./...` — conformance excluded via `conformance` build tag.
- Single test: `go test -race -run 'TestName(/subtest)?' ./internal/<pkg>/`.
- Conformance suite (real binary, real Postgres 16+): `go test -race -tags conformance -timeout 20m ./internal/conformance/...` Needs `IRIS_PG_DSN` pointing at cluster whose role has CREATEDB + CREATEROLE (see `.github/workflows/ci.yml`); without it, suite provisions embedded Postgres where possible, but CI parity = DSN path. Slow (~11m); don't run casually.
- Traceability gate: `go test ./internal/trace/...` (backlog mode, merge-blocking). Strict readout: `IRIS_TRACE_STRICT=1 go test -run TestTraceabilityGateStrict -v ./internal/trace/...`.
- Spec lock: gate fails when `docs/Iris Specification Inventory.md` drifts from `spec/inventory.lock`. After intentional spec delta (only alongside its test delta): `IRIS_TRACE_UPDATE_LOCK=1 go test -run TestSpecLockUpdate ./internal/trace`.
- Lint: `golangci-lint run` (config `.golangci.yml`; CI pins version in `ci.yml` — currently v2.12.2).

## Branching rules

- `master`: release line. Only receives epic-checkpoint PRs from `development`.
- `development`: integration line. All issue branches merge here.
- Issue branches: `issue/EXX.Y-short-name`, cut from `development`. PR title `EXX.Y <task name>`; PR body lists task's contract ids + Done-when checklist. Small tweaks may go on plain feature branches.
- Epic completes → PR `Epic EXX` goes `development` → `master`, waits for human review.
- Issue PRs merge on gates + CI green — no per-PR review step.

## Conventions

- Single Go module, application not library: all packages under `internal/`, only `cmd/iris` is main package. `spec/contracts.yaml` at repo root.
- Import graph one direction: `cli` → `daemon`/`api` → `dispatch` → `store`/`pg`/`exec`; `archive` beside `dispatch` reusing `store`/`pg`; `declare`, `build`, `pat` leaves.
- Plain idiomatic Go: gofmt/goimports/golangci-lint, `%w` wrapping, no cross-package panics, contexts through blocking calls, `slog` only, no mutable package globals, table-driven tests, doc comments on exported identifiers.
- Dependencies minimal, cgo-free: pgx, cobra, goccy/go-yaml, argon2id, embedded-postgres (or vendored equivalent). No ORM, migration framework, scheduler, SQLite, parquet, cloud clients.