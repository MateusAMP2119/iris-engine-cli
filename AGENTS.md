# Iris — engine + CLI

Provenance-first data engine + pipeline orchestrator, one Go binary (`cmd/iris`).
Epics + build order: `docs/Iris Epics.md`.

## Testing

The test suite is the durable asset; implementation is regenerable. Behaviour gets a test before it gets code.

1. Test tiers, cheapest first — write the test at the lowest tier that can actually observe the behaviour:
   - `unit` — pure logic, no I/O.
   - `integration` — fakes + local process I/O, no live Postgres.
   - `conformance` — real binary, running daemon, real Postgres.
   Unit + integration are what CI runs per Go version; conformance sits behind the `conformance` build tag.
2. Write the failing test first, then implement to green.
3. Never weaken a test to make it pass. If a test is genuinely wrong, change it deliberately as an intended behaviour change — never to silence a red run.
4. Nothing merges red: full suite green before merge.

## Commands

- Build: `go build ./...`; binary: `go build -o iris ./cmd/iris` (always cgo-free; release/cross-compile with `CGO_ENABLED=0`).
- Unit + integration (database-free, what CI runs per Go version): `go test -race ./...` — conformance excluded via `conformance` build tag.
- Single test: `go test -race -run 'TestName(/subtest)?' ./internal/<pkg>/`.
- Conformance suite (real binary, real Postgres 16+): `go test -race -tags conformance -timeout 20m ./internal/conformance/...` Needs `IRIS_PG_DSN` pointing at cluster whose role has CREATEDB + CREATEROLE (see `.github/workflows/ci.yml`); without it, suite provisions embedded Postgres where possible, but CI parity = DSN path. Slow (~11m); don't run casually.
- Lint: `golangci-lint run` (config `.golangci.yml`; CI pins version in `ci.yml` — currently v2.12.2).

## Branching rules

- `master`: release line. Only receives epic-checkpoint PRs from `development`.
- `development`: integration line. All issue branches merge here.
- Issue branches: `issue/EXX.Y-short-name`, cut from `development`. PR title `EXX.Y <task name>`; PR body lists a Done-when checklist. Small tweaks may go on plain feature branches.
- Epic completes → PR `Epic EXX` goes `development` → `master`, waits for human review.
- Issue PRs merge on CI green — no per-PR review step.

## Conventions

- Single Go module, application not library: all packages under `internal/`, only `cmd/iris` is main package.
- Import graph one direction: `cli` → `daemon`/`api` → `dispatch` → `store`/`pg`/`exec`; `archive` beside `dispatch` reusing `store`/`pg`; `declare`, `build`, `pat` leaves.
- Plain idiomatic Go: gofmt/goimports/golangci-lint, `%w` wrapping, no cross-package panics, contexts through blocking calls, `slog` only, no mutable package globals, table-driven tests, doc comments on exported identifiers.
- Dependencies minimal, cgo-free: pgx, cobra, goccy/go-yaml, argon2id, embedded-postgres (or vendored equivalent). No ORM, migration framework, scheduler, SQLite, parquet, cloud clients.