# Iris — engine + CLI

Provenance-first data engine + pipeline orchestrator, one Go binary (`cmd/iris`).
Reference docs: `docs/Iris Epics.md` (epics + build order).

## Commands

- Build: `go build ./...`; binary: `go build -o iris ./cmd/iris` (always cgo-free; release/cross-compile with `CGO_ENABLED=0`).
- Unit + integration (database-free, what CI runs per Go version): `go test -race ./...` — conformance excluded via `conformance` build tag.
- Single test: `go test -race -run 'TestName(/subtest)?' ./internal/<pkg>/`.
- Conformance suite (real binary, real Postgres 16+): `go test -race -tags conformance -timeout 25m ./internal/conformance/...` Self-sufficient: without `IRIS_PG_DSN` the suite boots ONE shared embedded Postgres for the whole run (first run downloads the pinned binary; no Docker, no system Postgres) and every leg runs. An ambient `IRIS_PG_DSN` (cluster role with CREATEDB + CREATEROLE) overrides it untouched — the CI path (see `.github/workflows/ci.yml`). Slow (~12m); don't run casually.
- Lint: `golangci-lint run` (config `.golangci.yml`; CI pins version in `ci.yml` — currently v2.12.2).

## Branching rules

- `master`: release line. Only receives epic-checkpoint PRs from `development`.
- `development`: integration line. All issue branches merge here.
- Issue branches: `issue/EXX.Y-short-name`, cut from `development`. PR title `EXX.Y <task name>`. 
- Small tweaks and experiments may go on plain feature branches.

## Conventions

- Single Go module, application not library: all packages under `internal/`, only `cmd/iris` is main package.
- Import graph one direction: `cli` → `daemon`/`api` → `dispatch` → `store`/`pg`/`exec`; `archive` beside `dispatch` reusing `store`/`pg`; `declare`, `build`, `pat` leaves.
- Plain idiomatic Go: gofmt/goimports/golangci-lint, `%w` wrapping, no cross-package panics, contexts through blocking calls, `slog` only, no mutable package globals, table-driven tests, doc comments on exported identifiers.
- Dependencies minimal, cgo-free: pgx, cobra, goccy/go-yaml, argon2id, embedded-postgres (or vendored equivalent). No ORM, migration framework, scheduler, SQLite, parquet, cloud clients.
