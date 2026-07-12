# Iris — engine + CLI

Provenance-first data engine + pipeline orchestrator, one Go binary (`cmd/iris`).
Reference docs: `docs/Iris Specification Inventory.md` (spec), `docs/Iris Epics.md` (epics + build order), `docs/Tasks/` (work items), `BUILD_STATE.md` (live build status).

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
- Issue branches: `issue/EXX.Y-short-name`, cut from `development`. PR title `EXX.Y <task name>`. Small tweaks may go on plain feature branches.
- Epic completes → PR `Epic EXX` goes `development` → `master`, waits for human review.
- Issue PRs merge on gates + CI green — no per-PR review step.

## Conventions

- Single Go module, application not library: all packages under `internal/`, only `cmd/iris` is main package. `spec/contracts.yaml` at repo root.
- Import graph one direction: `cli` → `daemon`/`api` → `dispatch` → `store`/`pg`/`exec`; `archive` beside `dispatch` reusing `store`/`pg`; `declare`, `build`, `pat` leaves.
- Plain idiomatic Go: gofmt/goimports/golangci-lint, `%w` wrapping, no cross-package panics, contexts through blocking calls, `slog` only, no mutable package globals, table-driven tests, doc comments on exported identifiers.
- Dependencies minimal, cgo-free: pgx, cobra, goccy/go-yaml, argon2id, embedded-postgres (or vendored equivalent). No ORM, migration framework, scheduler, SQLite, parquet, cloud clients.