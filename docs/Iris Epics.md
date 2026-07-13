---
status: draft
created: 2026-07-04
modified: 2026-07-04
tags:
  - iris
  - epics
  - planning
---

# Iris Epics

Derived from the [[Iris Specification Inventory]] (source of truth; on any conflict the spec wins). ==Seventeen== capability epics in build-dependency order. Every behavioral Q/A in the spec was decomposed into testable contracts per the spec's own testing doctrine: ==530 contracts from 123 Q/As (196 unit, 237 integration, 74 conformance, 23 exempt). Updated for the 2026-07-04 git-graph surface pass: `iris data why` renamed to `iris data provenance`, `iris data wipe` moved to `iris workload wipe [<pipeline>]`, the new `workload` noun, triage folded into the shows (the gate ledger in `pipeline show`, the blast radius in `deadletter show`, `run show --trace [--down]`), the `--graph` rendering contract, the `undo` enum's `skipped` split, the `run_inputs` reverse index, and the new read routes (E14 collects the new surface).==

## How to derive tasks

- A task is a small coherent cluster of contracts inside one epic (3 to 8 contracts is typical); the per-epic cutting notes suggest the seams.
- Every task follows the spec's TDD loop: add or confirm the contract rows in `spec/contracts.yaml`, write the failing tests at each contract's tier, implement to green, commit naming the satisfied contract ids.
- An epic is done when every non-exempt contract in its table is claimed by a green test and the traceability gate passes.
- Exempt contracts (naming, rationale, conventions) go into the manifest marked exempt; they gate nothing but keep coverage honest.
- Tier legend: unit = pure logic, no I/O; integration = fakes and local process I/O, no live Postgres; conformance = real binary, running daemon, real Postgres.
- The old GitHub tracker (iris.dw phases 1 to 6, issues #87 to #152) predates the spec pivot and is ignored here; reconcile it in a later pass.

## Epic map

| Epic | Name                                           | Contracts | Depends on                                                  |
| ---- | ---------------------------------------------- | --------- | ----------------------------------------------------------- |
| E00  | Conformance Harness and Traceability Gate      | 19        | none                                                        |
| E01  | Repo Skeleton, CLI Frame and Config            | 22        | E00 (contracts claimed as they turn green)                  |
| E02  | Engine Install, Daemon and Leadership          | 50        | E01                                                         |
| E03  | Declarations, Schemas and Apply                | 71        | E01 for pure parsing, E02 for apply against the daemon      |
| E04  | Roles, Grants and Credentials                  | 17        | E02, E03                                                    |
| E05  | Dispatcher, Lanes and Dead Letters             | 84        | E02, E03                                                    |
| E06  | Write Capture, Wipe and Promotion              | ==42==    | E03 (provisioning installs triggers), E05 (run attribution) |
| E07  | Provenance, Journal Lifecycle and Object Store | ==31==    | E05, E06                                                    |
| E08  | Build, Artifacts and Modes                     | 19        | E03, E05                                                    |
| E09  | Read API, Endpoints and PATs                   | 54        | E02 (server), E03 (schemas), E04 (roles)                    |
| E10  | Destructive Operation Gates                    | 16        | E03, E05, E06 (the operations it gates)                     |
| E11  | High Availability and Failover                 | 19        | E02, E05                                                    |
| E12  | Stats, Info and Inspect                        | 14        | E02, E05                                                    |
| E13  | Golden Sample and Acceptance                   | 31        | all others (the spine that proves them)                     |
| E14  | Graph Views and Triage Surface                 | 17        | E05, E07, E09                                               |
| E15  | Onboarding and Guided Tour                     | 14        | E02, E03, E05, E07 (the surfaces it tours)                  |
| E16  | Install Ceremony and Pipeline Catalog          | 10        | E15 (the tour it restructures)                              |

Build order is the table order ==with one exception: E14 builds before E13, the acceptance spine that proves everything above it, E14 included==. E00 is deliberately first: the harness and the red contract backlog exist before any product code, because the suite, not the implementation, is the durable asset.

## E00 Conformance Harness and Traceability Gate

**Goal.** Stand up the test system before any product code. The spec inventory is the source of truth and this suite is its executable form: the durable assets are the doc and the tests, the implementation is regenerable. Deliver the `spec/contracts.yaml` manifest (one row per contract in this document), the contract-claim convention (`// spec:` annotations and subtest paths), the CI traceability gate that fails on unclaimed contracts and on tests claiming no contract, golden-file infrastructure with an `-update` flag, the fixture set (golden workspace, deliberately invalid declarations), and the conformance runner that drives the real binary against a running daemon.

**Depends on.** none

**Cutting tasks.** Seed the manifest directly from the contract tables in this document. First green build: gate runs, every contract listed, every one unclaimed (the full red backlog is the deliverable).

**Contracts (12 testable, 7 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S16/acceptance-steps-cover-contracts` | Each numbered acceptance-scenario step serves as the conformance suite for its section's contracts, making 'the sample passes' and 'the spec is enforced' one statement (mapping doctrine). | exempt |
| `S16/archive-roundtrip-temp-files` | The archive format round-trips for real against temp files. | integration |
| `S16/ci-pipeline-merge-gate` | CI composition convention: GitHub Actions per push/PR runs the traceability gate, golangci-lint, database-free unit+integration on a Go 1.25/1.26 matrix, go build, and conformance against a Postgres service container (admin role carries CREATEDB, failover leg included); green across all stages including the gate is the merge gate; Windows is deferred from v1 (unix socket, service wrapper, process-group kill need Windows-specific answers) and revisited post-v1. | exempt |
| `S16/claims-via-subtest-or-annotation` | The gate recognizes a contract claim expressed either as a Go subtest path or as a // spec: annotation on the test. | unit |
| `S16/conformance-real-binary-json` | Conformance-tier tests drive the actual shipped binary against a running daemon over the socket, asserting exit codes and --json output, with a real Postgres created by the engine itself hosting both databases (the only tier where generated SQL meets a live database). | conformance |
| `S16/exempt-needs-no-test` | A contract explicitly marked exempt (non-behavioral: naming, rationale, philosophy) passes the traceability gate without any claiming test. | unit |
| `S16/fixture-inventory` | Fixture set convention: the golden workspace (sample project), valid and deliberately invalid declaration fixtures, and the rule that a golden-file diff is a contract diff and must ship with its spec delta. | exempt |
| `S16/gate-fails-unclaimed-contract` | Given a non-exempt contract in the manifest with no test claiming it, the traceability gate fails and reports that contract in the gap list (the TDD backlog). | unit |
| `S16/golden-byte-diff` | Every generated artifact (SQL/DDL, migration YAML, --dry-run previews, --json output) is compared byte-for-byte against a checked-in golden file and any difference fails the test. | integration |
| `S16/golden-update-flag` | Running the suite with the -update flag regenerates the golden files in place instead of failing on diff. | integration |
| `S16/integration-fakes-interfaces` | store, pg, and exec each sit behind an interface satisfied by a fake: a recording pg fake captures the exact CREATE/ALTER/GRANT and trigger DDL as golden files, a meta-store fake stands in for meta, and a fake process runner lets dispatch tests start runs in composer order, stream output, and cancel a run mid-flight with no real process or database. | integration |
| `S16/manifest-row-schema` | spec/contracts.yaml parses into one row per behavioral Q/A contract carrying a stable id (section + slug), doc anchor, tier, and status. | unit |
| `S16/no-fixed-sleeps` | Failover (and similar async) assertions wait on connection and run state, never on fixed sleeps (test-writing convention). | exempt |
| `S16/real-process-io-throwaway-scripts` | Process I/O needing no database (subprocess execution, output capture, cancel/kill, socket HTTP against the in-process daemon) is tested for real against throwaway scripts rather than fakes. | integration |
| `S16/spec-delta-without-test-fails-gate` | A spec delta (behavioral Q/A added or changed in the doc) without a corresponding test delta fails the traceability gate. | unit |
| `S16/spec-driven-doctrine` | Testing doctrine: the spec inventory is the source of truth and the suite its executable form; tests derive from Q/A entries before code exists, an implementation is correct exactly when the suite is green, and unenforced spec behavior counts as unspecified (philosophy). | exempt |
| `S16/tdd-loop-workflow` | TDD workflow convention: amend the Q/A first, add/update the contract row and derive failing tests, implement to green, commit names satisfied contract ids; an agent task is a spec section plus its failing tests and is done when they pass with no other contract broken; a code change without a spec delta must not alter test expectations (process rule, not machine-testable). | exempt |
| `S16/test-without-contract-fails-lint` | A test that claims no contract in the manifest fails the lint direction of the traceability gate (no invented behavior). | unit |
| `S16/tier-taxonomy-cheapest` | Three-tier taxonomy (unit, integration, conformance) organized by spec section with tier meaning execution only, the cheapest-proving-tier assignment principle, and the enumerated examples of which behaviors belong to each tier (doctrine). | exempt |

## E01 Repo Skeleton, CLI Frame and Config

**Goal.** One Go module, application layout under `internal/`, `cmd/iris` entrypoint, the full cobra noun-verb tree as stubs, global flags, exit-code categories, the `--json` output convention, config precedence (flags over env over `iris.toml` over defaults), lint and CI wiring, Go 1.25/1.26 matrix.

**Depends on.** E00 (contracts claimed as they turn green)

**Cutting tasks.** Cut tasks along: module and package layout; cobra tree plus exit codes; config precedence; CI plus lint. Stubs return `not implemented`, but exit codes and `--json` envelope are real from day one.

**Contracts (20 testable, 2 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S08/config-precedence-order` | Configuration resolves in strict precedence: command flags override IRIS_* environment variables, which override iris.toml values, which override built-in defaults. | unit |
| `S08/dl-sole-alias` | `iris dl` resolves to `iris deadletter` and is the only command alias in the tree. | unit |
| `S08/documented-env-vars-recognized` | Each documented environment variable (IRIS_SOCKET, IRIS_HOST, IRIS_TOKEN, IRIS_PG_DSN, IRIS_RETAIN, IRIS_JOURNAL_PARTITION_ROWS, IRIS_OBJECTS_PATH) is recognized and supplies the value for its corresponding setting. | unit |
| `S08/dry-run-only-on-declare` | `--dry-run` is accepted on `declare apply` and `declare destroy` and on no other command. | unit |
| `S08/exit-code-categories` | The shipped binary maps outcomes to categorical exit codes (0 success, 2 usage error, 3 no daemon, 4 operation failed, 5 dead-lettered, 6 not leader) with detail carried in the message or `--json`, never in extra codes. | conformance |
| `S08/exit3-no-daemon-guidance` | When no daemon is reachable a command exits 3 and its output includes guidance to start the engine. | conformance |
| `S08/global-flags-on-all-commands` | Every command accepts the global flags `--json`, `--socket <path>`, and `--host <addr>` + `--token <pat>` for remote daemon targeting over TCP. | unit |
| `S08/iris-toml-engine-settings-only` | iris.toml is limited to engine/connection settings and is never treated as a project manifest (project-level keys are not honored). | unit |
| `S08/json-single-envelope-stdout` | With `--json` every command emits exactly one structured JSON document (the read-API envelope) on stdout, while the default output is human-readable. | conformance |
| `S08/logs-separate-from-command-output` | Daemon and per-run logs are never interleaved into a command's stdout output stream. | integration |
| `S08/no-run-shaping-flags` | The CLI registers no run-shaping flags (`--param`, `--timeout`, `--retry`) on any command. | unit |
| `S08/resource-first-command-tree` | The CLI parses exactly the documented `iris <noun> <verb> [target]` resource-first command tree (declare, pipeline, run, data, ==workload,== engine, deadletter, endpoint, pat) with `list`/`show` ==plus `provenance` as the single computed-read verb== as the only read verbs and no flat verbs, mixed depth, or overloaded positionals. | unit |
| `S08/zero-config-defaults` | With no flags, env vars, or iris.toml present, the CLI defaults to the local socket and the engine defaults to managed Postgres. | unit |
| `S09/cgo-free-static-binary` | Building the engine with cgo disabled succeeds and yields a portable statically linked binary with no dynamic library dependencies. | integration |
| `S09/dependency-allowlist` | Direct dependencies are limited to the declared small set (pgx, cobra, goccy/go-yaml, argon2id, embedded-postgres or a vendored equivalent), with digests and signatures using only stdlib hashing and crypto/ed25519, and no ORM, migration framework, scheduler library, parquet library, or cloud object-store client anywhere in the module. | unit |
| `S09/go-version-floor-and-target` | The engine module compiles successfully under both the floor toolchain Go 1.25 and the target toolchain Go 1.26. | integration |
| `S09/pgx-only-no-sqlite` | All database access uses jackc/pgx directly (not through database/sql), and the module contains no SQLite driver dependency. | unit |
| `S10/code-style-conventions` | Code style follows plain idiomatic Go with gofmt/goimports/golangci-lint in CI, %w error wrapping without cross-package panics, context threading through blocking calls, slog-only logging, no mutable package globals, table-driven tests, and doc comments on exported identifiers (repo convention, non-behavioral). | exempt |
| `S10/import-graph-one-direction` | The internal package import graph is acyclic and flows one direction (cli to daemon/api to dispatch to store/pg/exec, archive beside dispatch reusing store/pg, declare/build/pat as leaves), verifiable by static import analysis. | unit |
| `S10/repo-layout-internal-only` | The engine repo is a single Go module (an application, not a library) with spec/contracts.yaml at the repo root, all packages under internal/, and cmd/iris as the only main package (repo convention, non-behavioral). | exempt |
| `S10/store-pg-sole-db-clients` | Only the store package opens the meta database and only the pg package talks to the data database; no other package (including archive) opens a third path to either database, verifiable by static import analysis. | unit |
| `S13/exit-json-contract-everywhere` | Every command's exit codes and --json output match the CLI contract. | conformance |

## E02 Engine Install, Daemon and Leadership

**Goal.** Everything between `iris engine install` and a leader dispatching: meta bootstrap DDL (17 tables, create-if-missing), managed and external Postgres on one code path, the admin-DSN resolution chain, unix socket plus opt-in TCP/TLS listeners, daemon foreground/detach lifecycle, signal handling, leader election on the advisory lock, the single-writer meta path, crash reconciliation, log rotation, service-unit install.

**Depends on.** E01

**Cutting tasks.** Cut tasks along: install/uninstall plus meta DDL; managed Postgres subprocess; DSN chain plus fail-fast guards; listeners plus CLI-daemon protocol; leader election plus single-writer store; crash reconciliation; logging plus service unit.

**Contracts (50 testable, 0 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S02/admin-dsn-memory-only` | The admin DSN is held only in daemon memory: never written to meta, never exposed to the CLI, and never stored by daemonless lifecycle commands resolving the same chain. | integration |
| `S02/admin-dsn-precedence` | The admin DSN resolves at startup as --pg-dsn over IRIS_PG_DSN over iris.toml pg_dsn, and with none present startup fails fast with no default. | unit |
| `S02/admin-dsn-privilege-check` | Startup validates the admin DSN holds CREATEROLE, CREATEDB, and managed-schema ownership, failing fast when a privilege is missing and never requiring superuser. | integration |
| `S02/cli-writes-via-leader` | The CLI never opens a connection to meta; every state change it requests is executed by the leader daemon over the control connection. | integration |
| `S02/connections-derive-admin-dsn` | Every Postgres connection the engine opens derives from the single daemon-owned admin DSN. | integration |
| `S02/daemon-log-rotation` | The daemon log rotates by size at 10 MB keeping 5 generations, with no time-based rotation ever. | integration |
| `S02/daemon-service-ready` | The detached daemon is service-ready: it runs without a TTY and exits with sane exit codes, so a systemd/launchd unit can wrap it directly. | conformance |
| `S02/daemonless-lifecycle-commands` | Only iris engine install, start, service install/uninstall, and uninstall on a stopped daemon are classified as runnable without a daemon. | unit |
| `S02/external-pg-identical-path` | With a user-provided pg_dsn the engine starts no local Postgres instance and drives the identical admin-DSN code path as managed mode. | integration |
| `S02/foreground-default-detach` | iris engine start runs the daemon attached in the foreground with streaming output by default, and -d detaches it so it keeps running after the CLI returns. | conformance |
| `S02/inflight-runs-deadlettered` | Reconciliation dead-letters leftover running runs with the reason that the daemon terminated while the run was in flight. | unit |
| `S02/leader-only-meta-writes` | Only the leader daemon writes meta, and every meta write is serialized through the single dispatcher-owned path. | integration |
| `S02/managed-pg-install` | iris engine install downloads the pinned, checksum-verified Postgres into .iris/pg/ with an engine-minted superuser the CLI never sees, using no Docker and no host packages. | conformance |
| `S02/managed-pg-subprocess-lifecycle` | In managed mode the daemon starts the local Postgres subprocess hosting both data and meta before dispatching any lane and stops it on shutdown, listening on a local socket by default with TCP only when standbys need it. | integration |
| `S02/meta-dedicated-db` | Engine bootstrap creates the dedicated meta database in the same cluster as data, with exactly 17 control tables in its public schema and no warehouse schemas. | integration |
| `S02/no-daemon-fail-fast` | A daemon-touching command invoked without a reachable daemon fails fast with guidance to start it and never auto-starts one. | integration |
| `S02/no-local-state-store` | All engine state lives in Postgres behind the single --pg-dsn covering both meta and data; the engine never creates SQLite storage or .iris/state.db. | integration |
| `S02/one-leader-sole-dispatcher` | With multiple daemon candidates running, exactly one acquires leadership via the advisory lock and only that leader dispatches runs. | integration |
| `S02/per-run-log-lifecycle` | Each run's log is written unrotated under .iris/logs/ keyed by run id, referenced from runs.log_ref, and deleted when its run row is pruned. | integration |
| `S02/pg-version-mismatch-fails` | When the data directory's recorded Postgres major version differs from the engine release's pinned version, startup fails fast instead of silently auto-upgrading. | unit |
| `S02/queued-runs-deleted` | Reconciliation deletes queued never-started runs rather than dead-lettering them, leaving the next pass to recreate them. | unit |
| `S02/readers-plain-mvcc-no-retry` | Meta readers use plain Postgres MVCC connections, and no code path performs busy-retry anywhere. | integration |
| `S02/reconcile-before-dispatch` | The leader completes startup reconciliation before dispatching any lane, using identical logic on cold start and failover. | integration |
| `S02/reconcile-no-journal-touch` | Reconciliation performs no journal step: capture rows stay where triggers wrote them, crashed disposable-run partials remain revertible, and nothing auto-replays. | unit |
| `S02/samehost-restart-kills` | On same-host restart, reconciliation best-effort SIGKILLs surviving process groups from recorded handles before disposing of their runs. | integration |
| `S02/service-install-on-demand` | iris engine service install generates a systemd/launchd unit wrapping the detached daemon, and no service unit or boot autostart is installed by any other command. | integration |
| `S02/signal-graceful-shutdown` | On SIGTERM or SIGINT the daemon stops dispatching, finishes or cleanly kills in-flight runs, flushes state, and exits cleanly. | conformance |
| `S02/standby-rejects-mutations` | A standby daemon rejects mutation requests with guidance pointing at the leader. | integration |
| `S02/structured-json-logs` | The daemon emits structured JSON logs via slog, switching to human-readable console output when running attached in the foreground. | integration |
| `S02/tcp-opt-in-pat-gated` | The TCP listener stays off unless enabled via --tcp or iris.toml, and every request over TCP must authenticate with a PAT. | integration |
| `S02/tls-when-certs-given` | The TCP listener serves TLS when --tls-cert/--tls-key (or iris.toml equivalents) are provided and falls back to plain TCP when they are absent. | integration |
| `S02/unix-socket-default` | The daemon always listens on a local unix socket protected by filesystem permissions and serves HTTP/JSON there with zero configuration. | integration |
| `S04/eighteen-table-roster` | Bootstrap DDL creates exactly eighteen engine tables: the seventeen listed meta tables plus public.data_journal in the data database. | integration |
| `S04/engine-key-public-via-info` | iris engine info exposes the engine key's public half while the private half stays stored in meta. | integration |
| `S04/fk-graph-matches-spec` | The bootstrap DDL's foreign-key edges exactly match the specified graph, with pipelines and runs as roots and migrations, run_summaries, journal_checkpoints, lanes, and data_journal carrying no FKs to the rest. | integration |
| `S04/install-bootstraps-engine` | iris engine install over the admin DSN creates meta if missing via plain CREATE DATABASE, ensures its tables, creates the partitioned public.data_journal, mints the ed25519 engine key, and sets up the socket. | integration |
| `S04/meta-ddl-create-if-missing` | Embedded meta DDL is applied create-if-missing at bootstrap and re-checked at each leader election, with no self-migration ledger or version gate. | integration |
| `S04/meta-readable-while-running` | Any Postgres client can read meta and the journal read-only while the daemon runs, without being blocked. | conformance |
| `S04/only-leader-writes-meta` | Only the elected leader daemon writes to meta. | integration |
| `S04/ordering-identity-never-clock` | Every engine-table ordering key is declared as a monotonic bigint identity column, with recorded_at kept as an opaque non-ordering audit string for log correlation only. | integration |
| `S04/state-split-meta-vs-data` | Engine control tables are created in the meta database while data_journal is created in the data database's public schema. | integration |
| `S04/uninstall-full-teardown` | iris engine uninstall drops meta and the data journal including all captured provenance, deletes the object store under objects_path, and removes the socket and service unit. | integration |
| `S06.1/dispatcher-sole-meta-writer` | All meta writes are issued through the single dispatcher goroutine; no other component writes meta. | integration |
| `S07/engine-uninstall-drops-endpoints` | iris engine uninstall drops meta and its endpoints together, so no endpoint outlives the engine. | integration |
| `S08/engine-start-owns-daemon-flags` | Daemon-scoped flags (`-d`, `--pg-dsn`, `--retain`, `--journal-partition-rows`, `--objects-path`, `--tcp`/`--tls-cert`/`--tls-key`) exist only on `iris engine start`. | unit |
| `S09/leader-lock-session-pinned-conn` | The leader-election session advisory lock is acquired and held on a session-pinned connection, never on a pooled connection whose return to the pool would release the lock. | integration |
| `S09/managed-postgres-subprocess` | The managed Postgres build is fetched and supervised as a child subprocess of the engine daemon, never linked into the engine binary. | conformance |
| `S10/iris-dir-default-paths` | The daemon defaults its unix socket to <workspace>/.iris/iris.sock, reads the optional .iris/iris.toml containing engine and connection settings only, and writes daemon.log plus per-run run-<id>.log files under .iris/logs/. | integration |
| `S10/managed-pg-under-iris-dir` | In managed mode the engine-managed Postgres, hosting the data database plus meta, is placed under <workspace>/.iris/pg/. | conformance |
| `S12/uninstall-drops-engine-state` | iris engine uninstall drops meta, the journal and its triggers, the object store under objects_path (artifact bytes and archived partitions), the socket, and the service unit. | integration |

## E03 Declarations, Schemas and Apply

**Goal.** The declared world: parse and validate `iris-declare.yaml` (eight fields, nothing else), lane composer rules and the 2+ interlock, `declare apply`/`destroy` single-file semantics with upstream-first registration and acyclicity, the `schemas/` tree, the closed YAML-to-Postgres type mapping, idempotent provisioning, additive-only migrations, schema and ledger drift detection.

**Depends on.** E01 for pure parsing, E02 for apply against the daemon

**Cutting tasks.** Parsing, validation, diffing and drift classification are pure unit logic and can start before E02 lands; apply/destroy against a live daemon closes the epic. The recording `pg` fake with golden DDL files carries most integration weight.

**Contracts (70 testable, 1 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S01/pipeline-folder-shape` | Declaration validation accepts a pipeline only as a folder named for the pipeline containing iris-declare.yaml plus one script, with no internal stage structure. | unit |
| `S03/apply-destroy-single-file` | iris declare apply and iris declare destroy each accept exactly one declaration file as target; workspace-wide or multi-file invocation is rejected. | unit |
| `S03/composer-file-shape` | A lane composer is an iris-declare.yaml in the lane folder one level above the pipeline folders it sequences, discriminated by content: a composer carries order (plus lane), a pipeline carries run. | unit |
| `S03/composer-lane-matches-folder` | A composer's lane value must match its lane folder name; a mismatch is rejected on apply. | unit |
| `S03/composer-required-at-two` | Apply rejects a lane reaching 2+ pipelines without a composer, while a single-pipeline lane is valid with no composer. | unit |
| `S03/dependencies-persist-rows` | Applied depends_on relationships persist as rows in the dependencies table in meta, separate from lanes. | integration |
| `S03/depends-on-cycle-rejected` | Apply rejects a depends_on graph containing a cycle (including self-reference) and names the offending chain in the error. | unit |
| `S03/eight-field-whitelist` | Declaration parsing accepts exactly the eight fields (name, run, env, env_file, lane, reads, writes, depends_on) and rejects any other key such as language, build, param, retry, schedule, triggers, executor, deadline, timeout, or state. | unit |
| `S03/inline-containment-agree` | When a pipeline joins a lane both inline (lane: <name>) and by folder containment, the two must name the same lane; a disagreement is rejected on apply. | unit |
| `S03/lanes-row-composer-written` | Lane state persists in lanes as name-keyed rows written whole only by the composer's own apply. | integration |
| `S03/name-matches-folder` | Apply rejects a declaration whose name does not match its pipeline folder name. | unit |
| `S03/name-required` | A declaration missing the required string field name is rejected on apply. | unit |
| `S03/omitted-lane-own-lane` | A pipeline that omits lane is placed in its own implicit lane, parallel with all other pipelines. | unit |
| `S03/order-entries-contained` | Apply validates that every composer order entry names a pipeline folder contained inside the lane folder. | unit |
| `S03/outside-member-rejected` | Inline lane: without folder containment is valid only while the lane is single-member; an apply that would create a 2+ lane with a member outside the lane folder is rejected with guidance to move the folder in. | unit |
| `S03/pipeline-single-lane` | Apply validates that each pipeline belongs to exactly one lane; membership in more than one lane is rejected. | unit |
| `S03/run-required-argv-list` | The run field is required and must parse as a plain string list (argv vector); a missing or non-list run is rejected. | unit |
| `S03/schemas-tree-shape` | The engine reads one schemas/ directory per workspace shaped as a folder per schema and per table, each table folder holding table.yaml (desired state) plus an engine-written migrations/ ledger. | unit |
| `S03/secrets-never-in-meta` | env and env_file values are resolved at run time only; apply persists the declaration to meta without storing resolved env or env_file values, so secret values never appear in meta rows. | integration |
| `S03/single-member-no-lanes-row` | A single-member lane (no composer) produces no lanes row; its name stays nominal until a composer apply promotes it to 2+. | integration |
| `S03/table-keys-match-folders` | Folder names under schemas/ are authoritative; schema:/table: keys are validated against them and a mismatch is rejected on apply. | unit |
| `S03/table-yaml-diff-writes-ledger` | Applying a changed table.yaml diffs it as desired head against the ledger and appends an engine-written, immutable migration file to migrations/ representing exactly that diff. | integration |
| `S03/unregistered-ref-rejected` | Apply rejects a depends_on reference to a pipeline not already registered (upstream-first, single-file), failing immediately rather than deferring resolution. | unit |
| `S04/declare-destroy-scoped-teardown` | iris declare destroy removes one declared unit at a time, leaving the engine and the schemas/ tree intact. | integration |
| `S04/dependencies-cross-lane-ok` | A depends_on edge between pipelines in different lanes is accepted as valid. | unit |
| `S04/dependencies-edge-shape` | dependencies stores one edge row per depends_on with from_pipeline/to_pipeline FKs to pipelines (from = dependent), composite PK on both columns, and indexes in both directions. | integration |
| `S04/lanes-composer-atomic-rewrite` | Composer apply replaces a lane's rows as one atomic full-lane rewrite, and pipeline applies never write the lanes table. | integration |
| `S04/migrations-ledger-shape` | migrations keys rows by (schema, table, migration_id) with parent, checksum, and applied_seq columns. | integration |
| `S04/migrations-record-applied-head` | Applying a table migration from schemas/ inserts a migrations row so each table's applied head is durably recorded for ledger-versus-disk drift detection. | integration |
| `S04/pipelines-table-shape` | pipelines DDL declares name as PK with folder, run (JSON argv), artifact constrained to (source, built), and data_mode constrained to (disposable, permanent). | integration |
| `S05/cross-mode-read-warns` | Apply warns but never refuses when a permanent-data pipeline declares reads on a disposable-mode pipeline's table (legitimate mid-promotion state). | unit |
| `S05/cross-mode-warning-rides-json` | The cross-mode read warning is carried in apply's --json output. | integration |
| `S05/drift-additive-only-autofix` | Across schema, ledger, and grant drift, only additive gaps are auto-resolved; every other discrepancy is reported without automatic action. | unit |
| `S05/ledger-drift-additive-generates-migration` | An additive gap between table.yaml and the migrations ledger generates the next numbered migration file and advances the ledger head. | integration |
| `S05/ledger-drift-removal-refused` | A column removed from table.yaml relative to the migrations ledger is classified non-additive and refused. | unit |
| `S05/migration-dry-run-touches-nothing` | Migration sync with --dry-run prints the intended ALTERs and migration files while executing no DDL and writing no files. | integration |
| `S05/migration-file-format` | A generated migration file records id, parent, op, the column definition, and a checksum of table.yaml at that revision. | unit |
| `S05/migration-sync-appends-and-alters` | Migration sync walks each table folder, diffs table.yaml against the ledger head, appends one immutable migration file per additive delta, and runs the corresponding ADD COLUMN ALTER. | integration |
| `S05/missing-capture-trigger-autofix` | A missing capture trigger on a declared table is classified additive and auto-fixed, like a missing column. | integration |
| `S05/modifier-ddl-rendering` | The four column modifiers (primary_key, nullable, default as raw SQL expression, unique) render into the corresponding PRIMARY KEY, NOT NULL, DEFAULT, and UNIQUE clauses in generated CREATE TABLE DDL. | integration |
| `S05/nullable-defaults-true` | A column parsed without an explicit nullable modifier is nullable by default. | unit |
| `S05/primary-key-implies-not-null` | A column marked primary_key is treated as not nullable even when nullable is unspecified. | unit |
| `S05/provision-applies-pending-migrations` | For a table that already exists, provisioning applies its pending additive migrations instead of recreating it. | integration |
| `S05/provision-create-if-missing` | Provisioning walks schemas/ and emits CREATE SCHEMA IF NOT EXISTS per folder and a CREATE TABLE from the table.yaml head for each missing table. | integration |
| `S05/provision-head-create-records-ledger` | When a table is created from its table.yaml head, the ledger head is recorded as applied in the migrations table. | integration |
| `S05/provision-idempotent` | Re-running provisioning against an already-provisioned target emits no schema, table, or migration changes. | integration |
| `S05/provision-one-path-per-table` | Provisioning selects exactly one path per table: create-from-head for missing tables XOR pending-migration replay for existing ones, never both. | unit |
| `S05/provision-pipeline-independent` | Provisioning is pipeline-independent: every table under schemas/ is included in the provisioning plan regardless of whether any pipeline declares reads or writes on it. | unit |
| `S05/schema-drift-excludes-engine-owned` | Capture triggers and the journal are excluded from the schema-drift comparison and are never flagged as drift. | unit |
| `S05/schema-drift-missing-column-autofix` | Schema drift classifies a column present in the declared head but missing live as additive and auto-fixes it with ADD COLUMN. | integration |
| `S05/schema-drift-nonadditive-refused` | Schema drift flags an extra, renamed, or retyped live column as non-additive and apply refuses without ever auto-dropping. | unit |
| `S05/table-ownership-model` | Ownership doctrine: schemas/ is the sole truth for declared tables while the engine owns the journal, capture triggers, and meta as undeclared non-user-editable surfaces. | exempt |
| `S05/type-mapping-closed-set` | Each YAML column type in the closed set maps to exactly its listed Postgres type (e.g. int to integer, double to double precision, bool to boolean). | unit |
| `S05/unknown-type-fails-apply` | A column declaring a YAML type outside the closed set fails apply validation. | unit |
| `S06.3/apply-atomic-registry-txn` | Registry changes commit in one dispatcher meta transaction, all-or-nothing; a validation failure changes nothing. | integration |
| `S06.3/apply-rejects-cycles` | Every apply checks acyclicity over the registered graph plus the new declaration and rejects a cycle by naming the chain. | unit |
| `S06.3/apply-single-file-resolution` | iris declare apply accepts exactly one iris-declare.yaml with no workspace sweep or transitive chaining, resolves a folder path to its file, and a bare apply is a usage error (exit 2). | unit |
| `S06.3/apply-upstream-first` | Apply rejects a declaration whose depends_on names an unregistered pipeline, and the rejection names the missing pipeline. | unit |
| `S06.3/composer-apply-atomic-lane-rewrite` | A composer apply rewrites the lane's entire order in one atomic all-or-nothing write, regardless of whether members are registered yet. | integration |
| `S06.3/member-apply-never-writes-lanes` | A member pipeline apply never writes lanes; a pipeline's position always comes from the composer's apply. | unit |
| `S06.3/two-plus-interlock` | A pipeline apply that would leave its lane folder with 2+ registered members is rejected, naming the lane, unless that lane's composer has been applied. | unit |
| `S08/declare-bare-usage-error` | Invoking `iris declare apply` or `iris declare destroy` without a path is a usage error (exit 2) and neither command offers an `--all` flag. | unit |
| `S10/workspace-tree-discovery` | Given a workspace tree, the engine discovers lane composers (pipelines/<lane>/iris-declare.yaml), pipeline declarations with their single script (pipelines/<lane>/<pipeline>/iris-declare.yaml plus main script), and table schemas (schemas/<schema>/<table>/ containing table.yaml plus migrations/) from their canonical locations. | unit |
| `S12/composer-destroy-interlock` | A composer declaration is destroyable only once its lane has at most one registered member, mirroring apply's 2+ invariant. | unit |
| `S12/destroy-retires-rows-one-txn` | declare destroy retires the pipeline's runs and inputs, dead-letter entries, artifacts and object-store bytes, dependency edges, lane rows, and role/grants/credentials in one meta transaction with the pipelines row deleted last. | integration |
| `S12/destroy-reverts-unpromoted-data` | declare destroy's teardown includes reverting the target pipeline's un-promoted disposable data along with its registration, role, and grants. | integration |
| `S12/destroy-single-declaration` | iris declare destroy accepts exactly one declaration file per invocation, so full teardown is one confirmed destroy per declaration in leaf-first order (mirroring apply's upstream-first). | unit |
| `S12/non-additive-refused-outright` | Non-additive schema changes are refused outright with no confirmation gate ever offered. | unit |
| `S13/apply-repeat-noop` | iris declare apply is idempotent: repeating an apply, including its schema provisioning, changes nothing. | conformance |
| `S13/apply-single-file-bare-exit-2` | iris declare apply is strictly single-file: exactly one declaration is accepted per invocation, and a bare invocation exits 2. | conformance |
| `S14/engine-column-refused-as-drift` | An engine-added column on a user table is classified as non-additive drift and refused, keeping table.yaml authoritative. | unit |

## E04 Roles, Grants and Credentials

**Goal.** Least-privilege plumbing: one engine-owned Postgres role per pipeline, the grants ledger in meta reconciled onto the data database, engine-managed credentials, scoped connection injection at spawn, grant drift detection, the reserved `public` schema bounds.

**Depends on.** E02, E03

**Cutting tasks.** Small and coherent; likely three tasks: role plus credential lifecycle; grant reconcile plus drift; connection injection.

**Contracts (17 testable, 0 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S02/meta-hidden-from-pipeline` | Pipeline roles and data PATs cannot access the meta database. | conformance |
| `S03/access-entry-table-fields-required` | Each reads/writes entry must name a dotted schema.table plus an explicit fields list; an entry omitting fields is rejected with no implicit all-columns fallback. | unit |
| `S03/apply-grants-exact-declared` | Apply issues grants on the pipeline's Postgres role for exactly the declared tables and fields, nothing more. | integration |
| `S03/apply-records-access-meta` | Apply records the declared reads/writes entries in meta. | integration |
| `S03/public-reads-writes-rejected` | Apply rejects any reads/writes entry targeting public.*. | unit |
| `S03/public-schema-folder-rejected` | Apply rejects a public schema folder under schemas/ (public is engine-reserved). | unit |
| `S03/reads-writes-no-ordering` | reads/writes govern access only and are not exclusive: overlapping entries between pipelines are accepted and create no dependency edges or run ordering, which derives only from lanes and depends_on. | unit |
| `S04/access-ledger-shape` | grants stores (pg_role, schema, table, field, access) rows indexed on pg_role, and roles maps each pg_role to exactly one owner, either a pipeline or a data PAT. | integration |
| `S04/credentials-pipeline-login-only` | credentials stores an engine-managed secret per login role and holds rows only for pipeline roles. | integration |
| `S04/ledger-truth-reconciled` | The access ledger in meta is authoritative and reconciliation emits grant DDL onto the data database to match it. | integration |
| `S05/grant-drift-reconcile` | Grant drift diffs live Postgres grants against the meta access ledger and reconciles so Postgres matches the ledger. | integration |
| `S05/stray-public-grant-nonadditive` | A live grant exceeding the ledger bounds (pipeline and data-PAT roles read-only on public, no connect to meta) is classified non-additive drift, reported, and never silently fixed. | unit |
| `S07/data-pat-grants-recorded-per-field` | Data-PAT grants are recorded per field in the access ledger at mint, even when minted via bare schema.table or --endpoint expansion. | integration |
| `S07/data-pat-no-post-mint-columns` | Grant reconciliation for a data PAT never grants columns added to the table after mint; the diff is computed against the ledger's fixed per-field grant set. | unit |
| `S07/pipeline-scoped-connection-injected` | Each pipeline run receives an engine-injected scoped connection for its least-privilege Postgres role at spawn, and database credentials never reach author or consumer hands. | integration |
| `S12/role-teardown-rides-destroy-uninstall` | Engine-managed pipeline role teardown executes only as part of declare destroy and engine uninstall, riding their confirmation gates rather than existing as a standalone operation. | integration |
| `S13/grants-postgres-enforced` | Postgres physically enforces each pipeline's grants: a read the pipeline did not declare fails at the database. | conformance |

## E05 Dispatcher, Lanes and Dead Letters

**Goal.** The engine's heart: subprocess execution (process groups, output capture, cancel and kill), run records and states, perpetual lane runners walking composer order, the `depends_on` gate with `run_inputs` consumption, lazy failure propagation, the dead-letter worklist with replay and drain, count-based retention with pruning into `run_summaries`, manual `pipeline run`, the clock doctrine and watermark/state-as-data doctrine.

**Depends on.** E02, E03

**Cutting tasks.** Biggest epic; cut along its natural seams: exec seam; run records plus states; lane loop plus pass semantics; gate plus consumption; propagation; dead-letter lifecycle (replay root-walk, drain, supersession); retention plus pruning plus summaries; manual run. Gate and propagation logic is pure unit work against the fake process runner.

**Contracts (79 testable, 5 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S01/cancel-kills-group-dead-letters` | iris run cancel kills the run's process group and dead-letters the run as stopped while touching nothing else. | integration |
| `S01/composer-order-no-data-link` | Composer order runs one pipeline after another within a lane purely as sequencing, with no data dependency and no failure propagation. | unit |
| `S01/dead-letter-single-terminal` | Every non-success ending (failed, stopped/cancelled, or upstream_dead_lettered) lands the run in the single dead-lettered terminal state and parks it in the dead-letter worklist. | unit |
| `S01/depends-on-eligibility-gate` | depends_on makes the downstream pipeline eligible only on the upstream's output, without imposing execution order. | unit |
| `S01/depends-on-failure-propagation` | An upstream failure propagates through depends_on to the downstream pipeline, dead-lettering it as upstream_dead_lettered. | unit |
| `S01/direct-exec-no-shell` | The engine starts a run by direct exec of the declared run command in its own process group, never through a shell, so pipes and globs in run are not interpreted. | integration |
| `S01/failure-never-globally-fatal` | A run failure is isolated: the engine keeps running and other lanes continue dispatching unaffected. | integration |
| `S01/hung-run-holds-lane` | A hung run holds its lane indefinitely, and the lane resumes only after that run is cancelled by the operator. | integration |
| `S01/no-auto-retry` | The engine never automatically retries a failed run; a dead-lettered run is not re-dispatched without explicit operator action. | unit |
| `S01/no-engine-timeout` | The engine never kills a run unilaterally; a run ends only by exiting on its own or by explicit cancellation. | integration |
| `S01/no-runtime-params` | The CLI accepts no --param flag or params-file, so the yaml declaration fully determines every run. | unit |
| `S01/run-cwd-env-injection` | A run's subprocess starts with working directory set to the pipeline folder and environment equal to inherited env plus declared env plus the injected scoped DB connection. | integration |
| `S01/run-handle-process-group` | The engine records the run's process-group id as its handle in runs.handle when the subprocess starts. | integration |
| `S01/run-output-captured` | The engine captures the subprocess's output for every run it starts. | integration |
| `S01/run-state-set` | A run moves only through the states queued, running, succeeded, and dead-lettered, with no other state values. | unit |
| `S03/cross-lane-gate-not-order` | A depends_on reference across lanes acts as a data gate (eligibility and propagation) without imposing any serial ordering between the lanes. | unit |
| `S03/env-file-fresh-per-run` | env_file KEY=VALUE files are re-read at each run dispatch by the leader, so a change to the file takes effect on the next run without re-apply. | integration |
| `S03/env-interpolation-merge` | env map entries resolve as literals or ${HOST_VAR} interpolations from the daemon environment and are merged onto the inherited environment. | unit |
| `S03/env-wins-over-env-file` | When a key appears in both env and env_file, the explicit env value wins in the resolved run environment. | unit |
| `S03/run-exec-no-shell` | In dev mode the run vector is executed as a direct argv with no shell interpretation, so shell metacharacters in arguments pass through literally to the process. | integration |
| `S03/run-records-hashes` | Every run row records the declaration checksum (runs.declaration_checksum), the binary hash, and the consumed upstream run ids. | integration |
| `S03/same-lane-serial-order` | Pipelines in the same lane are dispatched serially in the composed order while pipelines in separate lanes run in parallel. | integration |
| `S04/absent-pipeline-anonymous-lane` | A registered pipeline named in no lane is scheduled as its own anonymous lane. | unit |
| `S04/dead-letter-exit-paths` | A dead_letters row leaves the worklist only via replay (replacement run minted), supersession, or drain, while the run row remains in runs. | unit |
| `S04/dead-letters-worklist-shape` | dead_letters holds one row per outstanding dead-lettered run awaiting disposition with run_id as PK FK, reason in (failed, stopped, upstream_dead_lettered), error, and nullable failed_upstream. | integration |
| `S04/dev-run-null-artifact-hash` | A dev run's runs row records artifact_hash as null. | integration |
| `S04/failed-upstream-attribution` | Propagated dead-lettering records the immediate upstream whose dead-lettered run propagated in failed_upstream, while direct failures leave it null. | unit |
| `S04/gate-consumed-check-via-run-inputs` | The gate decides whether an upstream's latest success was already consumed by querying run_inputs, with no mutable cursor. | unit |
| `S04/lane-walk-skips-unregistered` | The runner walks a lane's rows ordered by pos and skips names with no registered pipeline. | unit |
| `S04/lanes-table-shape` | lanes DDL keys rows by (lane, pipeline) with UNIQUE(pipeline) and UNIQUE(lane, pos), referencing pipeline by name with no FK so an order may name unregistered folders. | integration |
| `S04/run-handle-crash-key` | A run's handle stores its subprocess process-group id and serves as the crash-recovery key. | integration |
| `S04/run-inputs-table-shape` | run_inputs DDL declares run_id and upstream_run_id as FKs to runs with a composite PK on both columns==, plus an index on upstream_run_id alone so downstream walks (run show --trace --down, the dead-letter blast radius) never seq-scan==. | integration |
| `S04/run-inputs-write-once` | Run start writes one run_inputs row per consumed upstream run (several under fan-in) and the rows are never mutated afterward. | integration |
| `S04/run-journal-window` | Dispatch stamps journal_floor with the journal high id at dispatch and the terminal transition stamps journal_ceiling, delimiting the run's journal window. | integration |
| `S04/run-snapshot-lsn` | Dispatch records the data-database LSN into the run's snapshot_lsn at dispatch time. | integration |
| `S04/run-summaries-outlive-pruning` | The pruner writes the summary row before pruning a run, and run_summaries is insert-only, FK-free, and never pruned. | integration |
| `S04/run-summary-construction` | A run summary copies run_id, pipeline name, state, artifact_hash, declaration_checksum, consumed upstream run ids (JSON), and the snapshot_lsn/journal_floor/journal_ceiling pin from the run being pruned. | unit |
| `S04/runs-table-shape` | runs DDL declares id bigint identity PK, pipeline FK, state in (queued, running, succeeded, dead_lettered), cause in (manual, loop, replay, propagated), nullable replayed_from self-FK to the replaced dead-lettered run, exit_code, handle, nullable artifact_hash FK to artifacts, declaration_checksum, log_ref, snapshot_lsn, journal_floor, journal_ceiling, recorded_at, and an index on (pipeline, id). | integration |
| `S06.1/clock-doctrine-no-time-ops` | Doctrine: no time-based operation anywhere; clocks never initiate, order, gate, bound, or end work; the sole wall-clock is the display-only uptime readout in iris engine info, and the hot idle loop is an accepted price kept cheap via watermarks. | exempt |
| `S06.1/dispatcher-post-pass-only` | Dispatcher-owned work (run records, dead-letter propagation, replay, snapshot pin, pruning, journal lifecycle) executes opportunistically only after a lane pass completes, never mid-pass. | integration |
| `S06.1/gate-never-reorders-walk` | A depends_on gate decides run-or-skip at the pipeline's composer-assigned turn and never changes its walk position. | unit |
| `S06.1/lane-runner-composer-order` | Each lane runner starts eligible pipelines in composer order (ORDER BY pos from lanes) on every pass. | integration |
| `S06.1/no-declaration-order-tiebreak` | Pipelines not ordered by a composer get no implicit declaration-order tiebreak and remain unordered in the walk. | unit |
| `S06.1/no-scheduler-doctrine` | Doctrine: orchestration is engine-owned; scripts never call each other or the engine, and no scheduler, cron, timer, or reactive trigger exists anywhere. | exempt |
| `S06.1/order-never-gates` | Composer order affects ordering only: a composer-ordered pipeline with no depends_on edge still runs when an earlier lane member fails. | unit |
| `S06.1/run-ends-only-exit-or-cancel` | A run never terminates on elapsed time; it ends only when its process exits or an explicit iris run cancel kills it. | integration |
| `S06.2/drain-pure-discard` | Drain discards the entry without re-running anything or altering downstream, makes the run prunable, and a drained run can never be replayed. | unit |
| `S06.2/failed-replay-chains-entry` | A replay that itself dead-letters parks a fresh dead_letters entry chained to the original via replayed_from, and the replay command exits 5. | unit |
| `S06.2/gate-awaits-latest-success` | B's gate opens on an upstream success not yet recorded in B's run_inputs and B records exactly that run 1:1, with the same rule for same-lane and cross-lane edges. | unit |
| `S06.2/multi-edge-all-resolve` | With several upstreams, B runs only when every depends_on edge resolves, recording one run_inputs row per edge. | unit |
| `S06.2/new-edge-awaits-next-success` | A newly added depends_on edge awaits the upstream's next success starting from pass one and never consumes historical runs. | unit |
| `S06.2/no-new-upstream-skips` | When the awaited upstream run is pending or nothing new exists since B last consumed, B gets no run row that pass. | unit |
| `S06.2/propagated-self-supersede` | A propagated dead-letter entry clears itself once its dependent consumes a later upstream run; only root causes (failed, stopped) require operator disposition. | unit |
| `S06.2/propagation-depends-edges-only` | Failure propagates only along depends_on edges, dispatcher-computed lazily at consumption time; composer-ordered neighbors and independent pipelines are untouched. | unit |
| `S06.2/propagation-writes-dead-run` | When B's awaited run is dead-lettered, the dispatcher writes B a never-executed dead-lettered run that pass with cause=propagated, reason=upstream_dead_lettered, failed_upstream set to the immediate upstream, and the poisoned run recorded in run_inputs. | unit |
| `S06.2/prune-cascades-inputs-and-log` | Pruning a run cascades to its run_inputs rows and deletes the run's log file. | integration |
| `S06.2/prune-never-touches-journal` | Run pruning never deletes data_journal rows; capture rows are bounded only by the journal's own lifecycle. | unit |
| `S06.2/prune-spares-outstanding-deadletter` | Post-pass pruning never removes a dead-lettered run with an outstanding dead_letters entry until replay, supersession, or drain releases it. | unit |
| `S06.2/prune-summary-same-txn` | The archival summary is written inside the same meta transaction that deletes the run, so surviving references never dangle. | integration |
| `S06.2/replay-dependents-not-forced` | Dependents of a replayed run follow via the normal depends_on gate on the next pass and are never force-run. | unit |
| `S06.2/replay-drain-bare-usage-error` | iris deadletter replay/drain invoked without <run>, --pipeline, or --all is rejected as a usage error (exit 2); nothing defaults to everything. | unit |
| `S06.2/replay-fresh-run-record` | A replay creates a fresh run on current data through the normal lane path at a run boundary with cause=replay and replayed_from set to the replaced run, and it becomes the pipeline's most recent run. | integration |
| `S06.2/replay-resolves-to-root` | Replay of a propagated entry walks failed_upstream to the root cause, and --pipeline/--all selections collapse to root causes. | unit |
| `S06.2/retention-count-based` | Retention is count-based and clockless: keep the newest retain runs per pipeline (default 1000, resolvable via --retain, IRIS_RETAIN, or iris.toml), with no consumer watermark ever pinning a run. | unit |
| `S06.2/superseded-not-owed` | Never-awaited upstream successes are superseded without backlog: B's next run reads everything skipped runs wrote, with no buffer or head-of-line blocking. | unit |
| `S06.2/transitive-propagation-immediate` | Propagation is transitive edge by edge: C dead-letters when awaiting B's dead-lettered run and records B, the immediate upstream, not the root. | unit |
| `S06.3/external-events-out-of-scope` | Scope statement: external event sources (files, webhooks, queues) are user code at the boundary, not an engine concern; the engine stays pure loop-plus-dependency. | exempt |
| `S06.3/lanes-parallel-serial-within` | Runs within a lane execute serially while distinct lanes run in parallel with no engine cap, and a laneless pipeline forms its own lane. | integration |
| `S06.3/pass-boundary-graph-visibility` | Runners read the walk at pass start: an in-flight pass finishes on the old graph, the next pass sees the new one, and in-flight runs are untouched. | integration |
| `S06.3/pass-fresh-run-no-retry` | Each pass starts every open-gated pipeline as a fresh run on current data; a failed run is never retried or backed off, and re-execution happens only via explicit replay. | unit |
| `S06.3/removed-pipeline-finishes` | A removed pipeline finishes its current run and then stops appearing in subsequent passes. | integration |
| `S06.3/row-safety-postgres-doctrine` | Doctrine: concurrent-write row safety, current-value (last committed writer), and read-modify-write atomicity come from ordinary Postgres MVCC, row locks, and the pipeline's own transaction; the engine adds no concurrency mechanism and no global cross-row order. | exempt |
| `S06.3/runner-skips-unregistered-names` | Lane walk construction skips lane-order names that have no registered pipeline. | unit |
| `S06.3/state-as-data-doctrine` | Doctrine: pipeline state (cursor, offset, high-water key) lives in a declared table the pipeline writes, never in an engine field, declaration slot, pipelines column, or meta KV store; the engine ships no row deltas. | exempt |
| `S08/deadletter-replay-deadletter-exit5` | `iris deadletter replay` exits 5 when a replayed run dead-letters again. | conformance |
| `S08/deadletter-scope-required` | `iris deadletter replay` and `iris deadletter drain` refuse a bare invocation, requiring an explicit `<run>`, `--pipeline <name>`, or `--all` scope. | unit |
| `S08/lane-member-manual-run-queued` | A manual `iris pipeline run` on a lane-member pipeline is queued as that lane's next run at the current run boundary (same-lane serialization preserved), while an own-lane pipeline runs immediately. | integration |
| `S08/manual-run-deadletter-exit5-cause-manual` | A manual `iris pipeline run` that dead-letters exits 5 and the resulting dead-letter entry records cause = manual. | conformance |
| `S08/manual-run-gate-consumption` | A manual pipeline run applies the depends_on gate exactly like a loop pass and, when eligible, consumes the upstream successes it ran against. | unit |
| `S08/manual-run-ineligible-exit4` | A manual `iris pipeline run` whose depends_on gate is not satisfied exits 4 with a reason explaining ineligibility. | conformance |
| `S08/pipeline-list-active-default` | `iris pipeline list` shows only pipelines with a queued or running run by default, and `--all` expands it to every registered pipeline. | integration |
| `S12/drain-discards-scoped-entries` | iris deadletter drain discards the outstanding dead-letter entries within the given scope and no others. | unit |
| `S12/drain-requires-explicit-scope` | iris deadletter drain refuses to run without an explicit scope argument (<run>, --pipeline, or --all). | unit |
| `S12/drained-prunable-not-replayable` | A drained dead-letter run becomes prunable and can never be replayed afterward. | unit |

## E06 Write Capture, Wipe and Promotion

**Goal.** Always-on provenance capture: the partitioned `data_journal`, statement-level triggers with transition tables, run attribution riding the injected connection, stamp versus pre-image payload tiers, ==`iris workload wipe [<pipeline>]`== reverse replay with the journal-internal conflict rule ==and the wiped/skipped disposition split==, promotion flipping wipe eligibility, the data-durability gate.

**Depends on.** E03 (provisioning installs triggers), E05 (run attribution)

**Cutting tasks.** Cut tasks along: journal DDL plus partitioning; trigger emission (golden PL/pgSQL); attribution; wipe replay plus conflict rule (pure unit on journal fixtures); promotion. The 1.25x bulk-capture budget lands in E13, not here.

**Contracts (==40== testable, 2 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S01/capture-covers-all-modes` | Capture is active in every artifact/data mode combination, with only the captured payload tier differing between modes. | integration |
| `S01/promote-capture-provenance-continue` | Capture and provenance continue unchanged after a pipeline's data is promoted to permanent. | integration |
| `S01/promote-ends-wipe-eligibility` | Promotion removes the pipeline's data from wipe eligibility and changes nothing else about the pipeline. | unit |
| `S02/journal-lives-in-data-db` | The capture journal is created as public.data_journal in the data database so triggers write it inside the data transaction, while all other engine state stays in meta. | integration |
| `S03/capture-adds-no-columns` | Capture-trigger installation adds no columns to declared tables (an engine-added column would be non-additive drift), so table.yaml remains the sole authority for table shape. | integration |
| `S03/capture-triggers-always-on` | Provisioning installs capture triggers on every declared table unconditionally; triggers cannot be declared, configured, or opted out of in the declaration. | integration |
| `S04/capture-in-data-transaction` | Capture triggers write data_journal rows inside the same data-database transaction as the captured write. | conformance |
| `S04/capture-row-per-write` | Every write by a run to a declared table produces exactly one data_journal row attributed to that run. | conformance |
| `S04/journal-run-id-not-fk` | data_journal.run_id references runs logically with no foreign-key constraint across the two databases. | integration |
| `S04/journal-select-only` | Every engine role may SELECT from public.data_journal and no role may write to it. | conformance |
| `S04/journal-table-shape` | data_journal DDL declares a bigint identity PK, pg_role, run_id, schema, table, row_pk, op in (insert, update, delete), pre_image, undo in (open, promoted, wiped==, skipped==), recorded_at, no post-image column, and exactly two indexes: the PK and the (schema, table, row_pk, run_id) provenance key. | integration |
| `S04/pre-image-wipe-eligible-only` | pre_image carries the prior row JSON only on wipe-eligible updates and deletes, and is null on inserts and on entries born promoted. | conformance |
| `S04/run-attribution-via-connection` | The run id set as a per-session setting on the injected connection at spawn is read by the capture trigger in-transaction, so no journal row is keyed to a role without a run. | conformance |
| `S04/statement-triggers-one-insert` | Capture triggers are statement-level with transition tables, issuing one INSERT...SELECT per statement regardless of row count, and only ever insert on the hot write path (no partitioning, sealing, or archiving inline). | integration |
| `S05/disposable-permanent-one-store` | One-store doctrine: disposable and permanent rows live in the declared tables themselves, with no separate schema, copy, or namespace and no seeding or mirroring; dev pipelines share upstream tables via depends_on exactly as production, isolation being logical (a row change is disposable exactly while its journal entries remain in wipe scope), and permanent rows derived from since-wiped inputs are the author's to reconcile. | exempt |
| `S05/post-promotion-writes-still-captured` | After promotion, new writes to the pipeline's tables are permanent yet still captured in the journal at stamp cost. | conformance |
| `S05/promotion-flips-open-to-promoted` | Promotion flips the pipeline's open journal entries to undo = promoted, and subsequent wipes skip them. | integration |
| `S05/promotion-no-data-movement` | Promotion mutates only undo markers and data_mode: it copies, moves, or deletes no table rows or journal entries. | integration |
| `S05/provision-ensures-capture` | Both provisioning paths end by ensuring capture: the partitioned journal exists once per data database and a capture trigger exists on every declared table. | integration |
| `S05/wipe-atomic-transaction` | Wipe executes in a single data-database transaction (journal and tables co-reside) so a mid-wipe failure leaves no partial wipe applied. | conformance |
| `S05/wipe-conflict-skip` | An open entry is conflict-skipped, its row left as-is, whenever any later journal entry exists for the same (schema, table, row_pk), with no image comparison, and the report names the conflicting run. | unit |
| `S05/wipe-never-clobbers-permanent` | Against a real database, wipe exactly reverts in-scope disposable rows while permanent writes are never clobbered or silently dropped. | conformance |
| `S05/wipe-retires-all-visited` | Wipe retires every visited open entry==, reverted ones to undo = wiped and conflict-skipped ones to undo = skipped==, so conflicts are reported once and never re-visited, and the summary reports both counts. | unit |
==| `S05/wipe-pipeline-scope` | A named `iris workload wipe <pipeline>` narrows the wipe scope to that pipeline's journal entries only, leaving other pipelines' open entries untouched; bare invocation covers the whole wipe scope, and declare destroy's data revert is exactly the narrowed form. | unit |==
| `S05/wipe-reverse-replay` | Wipe replays wipe-scope journal entries in reverse order, deleting disposable inserts and restoring pre-images for updates and deletes. | unit |
| `S05/wipe-scope-rule` | Wipe scope selects exactly the journal entries written under disposable data_mode that remain undo = open; promoted or wiped entries are excluded as provenance memory only and no command re-arms them. | unit |
| `S06.3/journal-row-commit-ordered` | Under concurrent same-table writes, journal ids per (schema, table, row_pk) are strictly commit-ordered because the capture trigger fires inside the writing transaction, so contested rows layer unambiguously for pre-images and ==iris data provenance==. | conformance |
| `S06.3/partial-writes-attributed-revertible` | A dead-lettered run's partial writes remain visible, are attributed via its journal_floor/journal_ceiling window, and are wipe-revertible when the run is disposable. | conformance |
| `S06.3/wipe-reverts-cursor-with-data` | Wiping a disposable run rolls its cursor-advance write back together with its data, so the next pass reprocesses exactly the reverted window and never silently skips it. | unit |
| `S12/capture-regardless-of-mode` | Every write is captured in the data journal regardless of the pipeline's data mode. | conformance |
| `S12/mode-change-wipe-eligibility-only` | Changing a pipeline's data mode changes wipe eligibility of its data but never changes capture behavior. | unit |
| `S12/unbuilt-permanent-write-refused` | The engine refuses permanent-mode data writes from a pipeline running un-built source; permanent data requires a built artifact. | unit |
| `S12/wipe-reverts-unpromoted-keeps-journal` | ==iris workload wipe== reverts un-promoted disposable data while retaining all journal rows. | conformance |
| `S14/capture-no-opt-out` | Capture is always on for every pipeline and role: provisioning installs capture triggers unconditionally and no configuration option disables capture. | integration |
| `S14/capture-overhead-budget` | A 10M-row promoted bulk insert completes within 1.25x of the capture-less baseline, gated by the acceptance scenario. | conformance |
| `S14/partition-by-id-range` | data_journal is partitioned by id range with partition size governed by the configurable count-based journal_partition_rows threshold (default 10M), treated as a threshold rather than an exact cap. | integration |
| `S14/preimage-only-where-undo` | Capture stores full pre-images only for writes where undo can spend them (wipe-eligible dev-loop writes); every other captured write records a slim stamp, never a row copy. | conformance |
| `S14/reads-not-captured` | Write triggers capture writes only; read-set provenance is declared out of scope for v1 (a north star, not a behavior). | exempt |
| `S14/run-stamps-one-partition` | All journal stamps for a single run land in one partition, so a run's journal window never splits across partitions. | unit |
| `S14/stamp-never-user-column` | Run attribution lives only in data_journal; engine-emitted DDL never adds stamp columns to user tables. | integration |
| `S14/wipe-promote-unsealed-only` | Wipe and promote touch only unsealed partitions; sealed journal history is immutable by construction. | unit |
| `S14/write-attributed-same-txn` | Every pipeline write to a captured table produces a journal stamp attributing it to its run, committed in the same transaction as the write itself. | conformance |

## E07 Provenance, Journal Lifecycle and Object Store

**Goal.** The read side of capture and its long-term shape: the ==`iris data provenance`== three-lookup walk, the snapshot pin, seal plus compaction (null released pre-images, fold duplicate stamps), the signed checkpoint chain, the content-addressed object store, partition export and archived reads, archival summaries keeping lineage whole after pruning.

**Depends on.** E05, E06

**Cutting tasks.** Cut tasks along: ==provenance== walk; pin; seal condition plus compaction; checkpoint chain plus engine key; object store plus export; archived reads. Digest chains and compaction collapse are pure unit logic; archive format round-trips against temp files.

**Contracts (==31== testable, 0 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S03/provenance-survives-pruning` | After run rows are pruned, provenance still names the exact declaration checksum and binary hash via the archival summary. | unit |
| `S04/checkpoint-ed25519-signature` | A checkpoint's signature is the engine key's ed25519 signature over its digest and verifies against the engine public key. | unit |
| `S04/checkpoint-parent-chain` | Each checkpoint's parent_digest chains to the prior checkpoint so tampering with or losing a sealed partition breaks chain verification visibly. | unit |
| `S04/checkpoint-per-sealed-partition` | Sealing a journal partition produces exactly one journal_checkpoints row with id_from/id_to and a digest hashed over the compacted rows in id order. | unit |
| `S04/checkpoints-insert-only` | journal_checkpoints is insert-only and never pruned, with location constrained to (resident, archived) and partitions referenced logically. | integration |
| `S10/objects-store-hash-keyed` | The object store at <workspace>/.iris/objects/ holds artifact bytes and archived journal partitions, keyed by hash. | integration |
| `S12/destroy-preserves-engine-journal` | After declare destroy the engine, the schemas/ tree, endpoints, and journal history all remain intact (provenance survives teardown). | integration |
| `S12/destroy-summaries-before-delete` | destroy writes each remaining run's archival summary before deleting the run, so journal stamps still resolve to a run or summary and provenance still names the binary after its bytes are gone. | integration |
| `S14/archive-file-format-roundtrip` | A sealed partition exports as one checksummed engine-owned file with a header carrying id range, digest, and signature, rows stored in id order, and round-trips through export and read-back exactly. | integration |
| `S14/archive-then-drop-flow` | After checkpoint, the engine exports the partition to the object store, re-validates the file digest, detaches and drops the partition, and flips the checkpoint location to archived. | integration |
| `S14/chain-detects-tamper` | Altering or losing any sealed or archived journal history causes checkpoint chain validation to fail visibly. | unit |
| `S14/checkpoint-digest-chain` | Sealing hashes the partition's compacted rows in id order, chains the digest to the previous checkpoint, signs it with the ed25519 engine key, and inserts the journal_checkpoints row. | unit |
| `S14/checkpoints-never-pruned` | journal_checkpoints is insert-only and its rows are never pruned by any retention policy. | unit |
| `S14/compaction-collapse-rule` | Compaction nulls pre-images past undo eligibility and collapses duplicate stamps per (schema, table, row_pk, run_id) to the latest op, while each run's exact write set survives compaction. | unit |
| `S14/engine-key-minted-at-install` | The ed25519 engine key used to sign checkpoints is minted at engine install time. | integration |
| `S14/meta-holds-no-payload-bytes` | meta stores only hashes and metadata for object-store contents, never payload bytes. | integration |
| `S14/missing-object-named-failure` | A host missing an object-store file still validates the checkpoint chain but fails the archived read with an error naming the missing hash. | integration |
| `S14/object-store-content-addressed` | The object store is a content-addressed directory of plain files at objects_path (default .iris/objects/) keyed by hash, holding artifact bytes keyed by binary content hash and sealed partitions keyed by checkpoint digest. | integration |
| `S14/objects-immutable-write-once` | Object-store files are immutable: written once, never rewritten, and deleted only by retirement or uninstall. | integration |
| `S14/offline-chain-validation` | An auditor holding only the archive files and the engine public key can validate the full checkpoint chain offline, with no Iris binary and no database. | integration |
| `S14/pin-recorded-dispatch-terminal` | At dispatch a run records the data database LSN as snapshot_lsn and the journal high id as journal_floor, and at terminal transition records the journal high id again as journal_ceiling. | integration |
| `S14/pin-survives-pruning` | run_summaries copies all three pin values (snapshot_lsn, journal_floor, journal_ceiling) so the pin remains queryable after the run row is pruned. | unit |
| `S14/seal-condition` | A journal partition seals only when it is past the row threshold, every in-flight run writing into it has finished, and it holds zero open entries. | unit |
| `S14/seal-dispatcher-step` | Sealing runs as an opportunistic dispatcher step after a pass, executing compact, checkpoint, then archive for each newly sealable partition. | integration |
| ==`S14/provenance-ancestry-recursive`== | Run ancestry is walked upward via run_inputs one row per consumed upstream, at depth 1 by default, with full recursive ancestry available in a single recursive query==, surfaced as iris run show --trace==. | unit |
==| `S14/provenance-current-author-surviving` | The provenance readout names the current author as the latest surviving stamp: a row whose newest entry is wiped resolves authorship to the latest non-wiped layer, and wiped layers stay listed in the output, never hidden. | unit |==
| ==`S14/provenance-cli-readout`== | ==iris data provenance <schema.table> <pk>== against the shipped binary reports the writing run and its state, artifact hash, declaration checksum, declared written fields, and consumed upstream runs for a row written by a real run. | conformance |
| ==`S14/provenance-lineage-never-images`== | The ==provenance== walk returns lineage only (stamps, run facts, ancestry) and never returns row images or pre-image payloads. | unit |
| ==`S14/provenance-row-to-run`== | Given a provenance key (schema, table, row_pk), the walk returns that row's stamps with the latest ==surviving== stamp naming the current authoring run and the full list giving the layered write history. | unit |
| ==`S14/provenance-run-facts-summary-fallback`== | The walk resolves a run id to its pipeline, state, artifact hash, declaration checksum, and pin, falling back to the archival summary once the run row has been pruned. | unit |
| ==`S14/provenance-spans-archive-boundary`== | The ==provenance== walk reads resident id ranges from Postgres and archived ranges from object-store files, returning equivalent results across the boundary. | integration |

## E08 Build, Artifacts and Modes

**Goal.** Artifact production and the mode matrix: one pinned recipe per language (Go, Python, Node), content addressing into `artifacts` plus object-store bytes, `iris pipeline build` and `promote` gating, the artifact/data mode matrix (source+permanent blocked), artifact retirement.

**Depends on.** E03, E05

**Cutting tasks.** Recipe inference and matrix validation are unit work; actual PyInstaller/pkg builds are conformance-tier and can ride E13.

**Contracts (19 testable, 0 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S01/apply-never-builds` | declare apply never triggers a pipeline build; building happens only via the explicit iris pipeline build command. | integration |
| `S01/artifact-data-mode-matrix` | Validation accepts source+disposable, built+disposable, and built+permanent, and blocks source+permanent so loose source never writes permanent data. | unit |
| `S01/both-modes-fully-wired` | Runs in both dev and built mode receive the injected scoped connection and full orchestration wiring. | integration |
| `S01/build-single-binary-content-hash` | iris pipeline build compiles the source into one self-contained binary and records its content hash so the executed bytes are always identifiable. | integration |
| `S01/build-toolchain-inferred-from-run` | The build recipe's toolchain is inferred from the pipeline's run command without any separate toolchain declaration. | unit |
| `S01/mode-selects-exec-target` | In dev mode the engine executes the pipeline source via its language runtime, and in built mode it executes the self-contained content-addressed binary instead. | integration |
| `S01/promote-requires-built` | iris pipeline promote marks data permanent only when the pipeline is built, and is rejected for a source-only pipeline. | unit |
| `S03/built-mode-ignores-run` | In built mode the engine executes the built binary directly and ignores the run vector. | integration |
| `S03/toolchain-inferred-from-run` | The engine infers the build toolchain from the run vector (e.g. [python, main.py] selects the python recipe) with no language or build field consulted. | unit |
| `S04/artifact-rebuild-inserts-new` | Artifact rows are immutable: a rebuild inserts a row under a new hash and the pipeline's current artifact is its newest row. | integration |
| `S04/artifact-retirement-post-prune` | After pruning, the dispatcher deletes artifact rows that are neither pipeline-newest nor referenced by a surviving run, together with their object-store objects. | integration |
| `S04/artifacts-row-is-index` | artifacts DDL declares hash PK, pipeline FK, size_bytes, and recorded_at; rows are index entries whose binary bytes live only in the object store under the hash, never as blobs in Postgres. | integration |
| `S05/promote-flips-data-mode` | iris pipeline promote flips the pipeline's per-pipeline data_mode in meta from disposable to permanent. | integration |
| `S05/promote-gated-on-built` | iris pipeline promote refuses when the pipeline is not in built state. | unit |
| `S05/promote-repeats-cross-mode-warning` | Promote repeats the cross-mode read warning while an upstream read dependency remains in disposable data_mode. | unit |
| `S09/build-recipe-not-declarable` | The build recipe is chosen by the engine from the pipeline's runtime and cannot be declared or overridden in the pipeline declaration. | unit |
| `S09/build-records-hash-and-bytes` | A successful pipeline build records the produced binary's content hash in the artifacts table and stores the binary's bytes in the object store. | integration |
| `S09/pinned-build-recipe-per-runtime` | Building a pipeline selects exactly the one pinned recipe for its runtime: Go via native go build, Python via PyInstaller one-file, Node via pkg, with no menu of alternatives. | unit |
| `S09/unsupported-runtime-build-error` | Attempting to build a pipeline whose runtime has no pinned recipe fails with an "unsupported runtime" error. | unit |

## E09 Read API, Endpoints and PATs

**Goal.** The network surface: PAT mint/verify with argon2id and show-once, scope checks per route, data-PAT NOLOGIN roles assumed via SET ROLE on a shared read pool, the full GET route set, endpoint YAML lifecycle with deterministic SQL compilation and prepare-verification, the wire contract (param grammar, envelope, closed error codes, keyset pagination, per-type serialization), NDJSON streaming.

**Depends on.** E02 (server), E03 (schemas), E04 (roles)

**Cutting tasks.** Cut tasks along: PAT store plus scopes; route mux plus auth split; endpoint compile plus lifecycle; /q and /data execution path; wire contract (largest test surface, mostly unit against the compiled shapes plus socket HTTP integration); NDJSON.

**Contracts (54 testable, 0 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S04/data-pat-owns-read-role` | Granting a PAT the data scope records an engine-managed read-only Postgres role for it in the access ledger. | integration |
| `S04/data-pat-role-nologin-set-role` | Data-PAT roles are created NOLOGIN with no credentials row and are assumed via SET ROLE on the API read path. | integration |
| `S04/endpoint-authority-calling-pat` | Endpoints own no roles or credentials; a read request executes under the calling PAT's role. | integration |
| `S04/endpoints-table-shape` | endpoints DDL declares name PK, dotted schema.table source, JSON fields projection, and a unique-column keyset sort key; endpoint_filters keys (endpoint, param) with op in (eq, range). | integration |
| `S04/pat-authority-scope-union` | A PAT's effective authority is computed as the union of its pat_scopes rows. | unit |
| `S04/pat-store-shape` | pats keys rows by token prefix with an argon2id hash, label, and revoked flag; pat_scopes stores one row per scope with scope in (control, read, data) and PK (pat_id, scope). | integration |
| `S07/cli-api-same-views` | CLI readouts print the same curated views the corresponding API routes serve. | conformance |
| `S07/collection-key-roster` | Cursor keys are fixed per collection: monotonic id for runs, dead_letters, and the journal; name for pipelines; (lane, pos) for lanes; the table PK for /data; the endpoint sort field for /q. | unit |
| `S07/column-type-serialization` | Values serialize per column type: int/bigint/smallint/double as JSON numbers, numeric as string, bool as JSON boolean, text/varchar/uuid as strings, timestamptz/timestamp/date/time as RFC 3339 strings, json/jsonb inline, bytea as base64, and recorded_at audit strings opaque, never interpreted for ordering. | unit |
| `S07/data-pat-grant-resolution` | At mint, --read resolves field-explicit grants, bare schema.table expands to all fields declared at that moment, and --endpoint <name> expands to that endpoint's source fields. | unit |
| `S07/data-pat-reads-physically-bounded` | Postgres physically bounds a data PAT's reads to its granted fields. | conformance |
| `S07/data-pat-role-nologin-no-credentials` | A data PAT maps to an engine-managed read-only NOLOGIN Postgres role assumed via SET ROLE, and gets no row in credentials, which holds pipeline roles only. | integration |
| `S07/data-raw-route` | /data/{schema}/{table} serves ad-hoc reads with column projection, eq/range filters, and keyset paging by the table PK. | integration |
| `S07/disposable-rows-visible` | Data-surface reads return disposable rows unfiltered; no route filters them out. | integration |
| `S07/endpoint-apply-live-on-commit` | An applied endpoint takes effect on commit and serves requests without a daemon restart. | integration |
| `S07/endpoint-apply-verify-persist` | iris endpoint apply prepare-verifies the derived SQL against the data database and persists to endpoints + endpoint_filters atomically, all-or-nothing. | integration |
| `S07/endpoint-filter-sort-validation` | Endpoint apply accepts only eq or range as a filter kind and requires sort to be a unique source column. | unit |
| `S07/endpoint-lifecycle-independent` | iris endpoint apply and iris endpoint remove publish and retire endpoints independently of declare apply, and declare destroy leaves tables and endpoints standing. | integration |
| `S07/endpoint-reapply-boundary-swap` | Re-applying an endpoint swaps its shape at a request boundary; in-flight requests finish with their starting shape. | integration |
| `S07/endpoint-single-table-projection` | An endpoint declares an explicit field projection over one table only; joins, aggregations, and computed fields are rejected. | unit |
| `S07/endpoint-source-validation` | Endpoint apply resolves the single declared source table against schemas/ and refuses the journal as a source. | unit |
| `S07/endpoint-sql-deterministic` | Apply derives exactly one parameterized SQL text deterministically from an endpoint YAML; identical input yields byte-identical SQL. | unit |
| `S07/endpoint-yaml-file-shape` | An endpoint is one flat file at endpoints/<name>.yaml and apply rejects a file whose filename does not equal its endpoint: field. | unit |
| `S07/engine-state-route-roster` | The engine-state surface serves exactly the meta-roster routes (/pipelines, /runs, /dead_letters, /lanes, ==/dependencies,== /leader, /stats, /healthz, /provenance/{schema}/{table}/{pk}) with their item sub-routes, all GET==, plus the graph and triage routes owned by E14==. | integration |
| `S07/engine-storage-unreachable` | Engine storage is unreachable through the data surface: meta is a separate database that /data and /q cannot address. | integration |
| `S07/eq-range-grammar` | eq filters bind as <param>= and range filters bind as <param>_from/<param>_to with either side optional and both bounds inclusive. | unit |
| `S07/error-envelope-closed-codes` | Errors reuse the same envelope with an error object whose code comes only from the closed set {bad_param, unauthorized, forbidden, not_found, method_not_allowed, internal}. | unit |
| `S07/exact-field-filtering` | Collection routes filter on exact field equality only (e.g. /runs?pipeline=load_orders&state=dead_lettered). | unit |
| `S07/healthz-leader-report-role` | GET /healthz and GET /leader report the node's current role on both leader and standby. | integration |
| `S07/http-get-json-both-listeners` | The daemon serves the read API as HTTP/1.1 resource-shaped JSON GETs on the same server as the control plane, reachable over both the unix socket and the optional TCP listener. | integration |
| `S07/http-status-matrix` | Status codes map exactly: 200 success, 400 malformed/unknown/repeated param, 401 missing or bad token on TCP, 403 missing scope or grant, 404 unknown endpoint, 405 non-GET, 500 engine fault. | integration |
| `S07/keyset-cursor-paging` | Pagination is keyset-cursor, always ascending by the collection key ==(one bounded exception: id-keyed collections also take before=, a reverse cursor for newest-first log pages, still keys, never clocks)==, driven by ?after=<key>&limit=<n> (after is a raw key value parsed like any param) and page.next_after, with no offset or since-timestamp paging. | unit |
| `S07/limit-default-cap` | limit defaults to 100 and caps at 1000; an over-cap value yields 400, never a silent clamp. | unit |
| `S07/ndjson-resume-by-cursor` | A dropped NDJSON stream resumes from its last received row by passing that row's key as the cursor on the same route. | integration |
| `S07/ndjson-streaming` | Every collection route requested with Accept: application/x-ndjson streams one JSON row per line with no envelope through the end of the result, on the same routes with the same auth. | integration |
| `S07/no-caller-sql` | The surface executes only engine-built statements with bound params (compiled text for /q, assembled from validated identifiers for /data); caller input never becomes SQL text. | unit |
| `S07/param-type-parse-400` | A param value that fails to parse per its source-column YAML type yields 400 naming the param. | unit |
| `S07/pat-scope-subset-validation` | iris pat create accepts any non-empty subset of {control, read, data} as scopes and rejects an empty or unknown scope set. | unit |
| `S07/pat-show-once-hash` | PAT creation prints the full token exactly once and persists only prefix plus argon2id hash, so a lost token can only be revoked and re-minted, never recovered. | integration |
| `S07/provenance-route-lineage-only` | GET /provenance/{schema}/{table}/{pk} returns lineage under the read scope alone==, carrying what data provenance prints (the full layer list with per-stamp disposition),== and never includes row images. | integration |
| `S07/q-caller-role-execution` | /q/{endpoint} authorizes with the data scope and executes as the caller PAT's role, never an endpoint-owned role (endpoints own no roles and mint no credentials). | integration |
| `S07/q-forbidden-names-endpoint` | When the caller's role lacks a grant on an endpoint's source fields, Postgres refuses the read and the API returns 403 forbidden naming the endpoint, never the missing fields. | conformance |
| `S07/read-pool-set-role-cycle` | Each read checks out a shared-pool connection on the data database, runs SET ROLE <pat_role>, executes a single-statement read-only transaction, and RESET ROLE on release; the same mechanics serve /data routes. | integration |
| `S07/read-surface-no-writes` | No route on the read surface can mutate: the journal is readable but never writable and no writes or DDL are reachable through the API; all changes go via the control plane. | integration |
| `S07/request-time-prepared-statements` | Request handling never assembles SQL: each pooled connection prepares the fixed statement text on first use (session-scoped prepared statements) and executes with bound params. | integration |
| `S07/response-envelope` | Successful responses use the { "data": [...], "page": { "next_after": <key\|null>, "limit": <n> } } envelope with rows mirroring source columns. | unit |
| `S07/scope-split-403` | A data-only PAT receives 403 on engine-state routes and a read-only PAT receives 403 on /data and /q routes; every route is scope-checked. | integration |
| `S07/standby-serves-reads` | A standby daemon serves all read routes from the shared meta; reads work on any candidate regardless of role. | integration |
| `S07/transport-auth-socket-vs-tcp` | Socket requests are ambiently authorized while TCP requests require a per-request Authorization: Bearer token, returning 401 when the token is missing or bad. | integration |
| `S07/unknown-repeated-param-400` | An unknown or repeated param yields 400 and is never silently ignored. | unit |
| `S10/api-cli-read-render-parity` | For every read surface, the HTTP api handler returns exactly the same content the corresponding CLI read command renders, proven by comparing GET responses from an in-process daemon against CLI output. | integration |
| `S10/endpoint-files-canonical-location` | Declared read endpoints are discovered as flat, shape-only YAML files at endpoints/<name>.yaml in the workspace. | unit |
| `S11/stats-read-pat-access` | A monitor holding a read-scoped PAT can successfully fetch GET /stats. | integration |
| `S14/provenance-http-endpoint` | GET /provenance/{schema}/{table}/{pk} serves the same provenance walk result over the HTTP API as the CLI walk returns. | integration |

## E10 Destructive Operation Gates

**Goal.** The five gated destructive operations, tiered confirmation (type-the-name versus y/N), soft-blocks with `--yes`/`--force` semantics, destroy blocker rules (dependents, run_inputs references, dead-letter references), remote-surface tiering (uninstall never remote), failover never resuming an interrupted destructive op.

**Depends on.** E03, E05, E06 (the operations it gates)

**Cutting tasks.** Gate and blocker predicates are unit logic; the confirmations and remote tiering close at conformance.

**Contracts (16 testable, 0 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S08/force-overrides-soft-blocks` | On destructive commands `--force` overrides soft-blocks, allowing a soft-blocked operation to proceed. | unit |
| `S08/yes-honors-soft-blocks` | On destructive commands `--yes` satisfies the confirmation prompt but a soft-blocked operation still refuses to proceed. | unit |
| `S12/api-destructive-control-pat-confirm-field` | Destructive ops invoked via the API require a control PAT plus an explicit confirm body field. | integration |
| `S12/api-destructive-leader-only` | Destructive ops invoked via the API are accepted by the leader only. | integration |
| `S12/destroy-downstream-blockers` | declare destroy refuses and names the blockers (drop or drain first) while any registered pipeline declares depends_on on the target, any downstream run_inputs row names the target's runs, or any outstanding dead-letter entry names it as failed_upstream. | unit |
| `S12/destructive-ops-tcp-reachable` | declare destroy, ==workload wipe==, and deadletter drain are reachable over the TCP listener. | integration |
| `S12/devloop-yn-confirm` | Dev-loop destructive ops (==workload wipe==, deadletter drain) confirm with a y/N prompt rather than typed-name confirmation. | integration |
| `S12/dry-run-writes-nothing` | --dry-run on declare apply and declare destroy prints a preview of the operation and writes nothing. | integration |
| `S12/failover-no-resume-destructive` | A new leader after failover never resumes an interrupted destructive op; the caller must re-issue and re-confirm it. | integration |
| `S12/five-ops-confirmation-gated` | Each of the five destructive operations (declare destroy, engine uninstall, ==workload wipe==, deadletter drain, role teardown riding the first two) refuses to execute without explicit confirmation. | integration |
| `S12/force-cancels-inflight` | --force overrides the soft-blocks and cancels in-flight runs on the affected scope, dead-lettering the cancelled runs as stopped. | integration |
| `S12/teardown-typed-name-confirm` | Irreversible teardowns (engine uninstall, declare destroy) print what they will remove and, on a TTY, require typing the target name to confirm. | integration |
| `S12/uninstall-local-only` | iris engine uninstall is local-machine-only with no listener path (daemonless, never remote), so a leaked control PAT cannot trigger it remotely. | integration |
| `S12/uninstall-refuses-live-candidate` | engine uninstall refuses while any daemon candidate still holds a meta connection, so a shared meta is never dropped under a live candidate. | integration |
| `S12/yes-soft-block-inflight-run` | Non-interactive --yes confirms the gate but still refuses with guidance while a run is in flight on the affected scope. | unit |
| `S12/yes-soft-block-unpromoted-data` | Non-interactive --yes on a teardown (engine uninstall, declare destroy) refuses with guidance while un-promoted disposable data exists. | unit |

## E11 High Availability and Failover

**Goal.** Availability of the one meta: standby candidates blocking on the advisory lock, promotion running the same reconciliation as restart, explicit self-demotion on session loss (stop dispatching, kill in-flight), workspace-tree and object-store per-host prerequisites, standby mutation rejection with leader guidance.

**Depends on.** E02, E05

**Cutting tasks.** Leader-lock logic against the meta-store fake; one real conformance leg (two candidates, kill the leader) rides E13 step 9.

**Contracts (16 testable, 3 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S02/crosshost-failover-no-kill` | On cross-host failover the new leader dead-letters in-flight runs without killing their processes, relying on the deposed leader's self-demotion to kill them. | integration |
| `S07/standby-mutations-rejected-exit-6` | A mutation attempted against a standby is rejected with guidance to the leader and exit code 6; only the leader accepts mutations. | conformance |
| `S08/exit6-names-leader` | A command sent to a non-leader daemon exits 6 and the message/`--json` output names the current leader for retargeting. | conformance |
| `S15/advisory-lock-leader-election` | Leadership is a Postgres session advisory lock: a standby blocked acquiring the meta leader lock acquires it and becomes leader when the previous holder's session ends. | integration |
| `S15/candidate-requires-workspace-tree` | A daemon candidate started on a host lacking the workspace tree the leader dispatches from (pipeline folders, dev source, env_files) refuses to start. | integration |
| `S15/failover-leader-own-objects-path` | A leader promoted by failover dispatches built runs using artifact bytes resolved from its own objects_path. | integration |
| `S15/failover-standby-takes-over` | With two real daemon candidates sharing one meta, killing the leader causes the standby to acquire the advisory lock and take over as the dispatching leader. | conformance |
| `S15/failover-stopped-runs-dead-letter` | Runs killed by a failover are dead-lettered and poison their dependents' next consumption until an explicit iris deadletter replay unsticks the chain. | integration |
| `S15/managed-pg-dies-with-host` | Documented limitation: managed Postgres is a daemon subprocess that dies with its host, so meta availability beyond one host requires external mode (non-behavioral scope statement). | exempt |
| `S15/no-meta-write-without-lock` | meta writes are never issued over a session that has not re-acquired the leader lock, so no second writer and no overlapping run can exist across a failover. | integration |
| `S15/no-scale-out-by-design` | Design scope statement: runs execute only as subprocesses on the leader's host; scaling levers are lanes and host capacity, and remote execution is out of scope (non-behavioral). | exempt |
| `S15/postgres-is-the-scaling-ceiling` | Scope statement: the remaining availability/scale ceiling is Postgres itself; the engine is indifferent to where pg_dsn points (managed local, RDS, Aurora, Citus), and meta availability and data-plane scale are bought with a better Postgres, not engine work (non-behavioral). | exempt |
| `S15/promotion-runs-startup-reconciliation` | On acquiring leadership, the newly promoted daemon runs the same startup reconciliation it would run after a restart. | integration |
| `S15/reads-work-on-any-candidate` | Every candidate binds its own listeners and serves read requests regardless of leadership role. | integration |
| `S15/self-demotion-on-session-loss` | A daemon that loses its meta session immediately self-demotes: it stops dispatching, kills in-flight runs, and re-enters standby on a fresh session. | integration |
| `S15/standby-mutation-exit-6` | A mutation command run via the shipped CLI against a standby daemon exits with code 6. | conformance |
| `S15/standby-rejects-mutations-with-leader-guidance` | A mutation request sent to a standby daemon's listener is rejected with an error carrying guidance pointing to the current leader. | integration |
| `S16/failover-lock-fake` | Against a meta-store fake, a standby candidate blocks while the leader lock is held and is promoted when the lock is released. | integration |
| `S16/failover-real-leader-kill` | With two real daemon candidates sharing one meta, killing the leader results in the standby taking over. | conformance |

## E12 Stats, Info and Inspect

**Goal.** Read-only readouts: `iris engine stats` rollups (identical over CLI and GET /stats), `info` (role, listeners, key, uptime as the sole wall-clock), `inspect` DDL dump, clock-free counters, no metrics endpoint in core.

**Depends on.** E02, E05

**Cutting tasks.** Small; one or two tasks once their source data exists.

**Contracts (14 testable, 0 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S11/info-readout-fields` | `iris engine info` reports engine and Go version, socket and TCP listeners, data and meta targets, leader/standby role (naming the leader when known), objects path, the engine key's public half, per-lane pass counts, and uptime. | integration |
| `S11/inspect-dumps-engine-ddl` | `iris engine inspect` dumps the engine-table DDL as a read-only operation, mutating no engine state. | integration |
| `S11/lane-pass-counter-reset` | The per-lane loop pass counter is a leader-held runtime counter that resets on daemon restart and on leader change. | integration |
| `S11/no-liveness-readouts` | No readout (stats, info, inspect, show) contains a last-heartbeat or last-seen field; connection state is the only liveness signal. | unit |
| `S11/no-metrics-endpoint` | The daemon exposes no /metrics endpoint in core; a request to /metrics returns not-found rather than a metrics document. | integration |
| `S11/pipeline-show-readout` | `iris pipeline show` reports the pipeline's resolved declaration, its role and grants, ==its== recent runs==, and the gate ledger (per-edge verdict from the closed set)==. | integration |
| `S11/stats-cli-http-parity` | GET /stats and `iris engine stats` return the identical read-only rollup payload for the same engine state. | integration |
| `S11/stats-clock-free` | Every stats value is a current count or last-value; the payload exposes no time-series and no clock-derived metric (the pass counter is a count, not a duration). | unit |
| `S11/stats-engine-rollup` | The stats payload's engine-wide rollup reports dead-letter worklist depth and counts by reason, running runs, capture counters, the wipe-eligible slice, total journal size, and the lifecycle readout of hot rows, sealed and archived partition counts, and checkpoint chain head. | integration |
| `S11/stats-lane-rollup` | The stats payload's per-lane rollup reports pipeline count, queued/running count, and loop passes completed since daemon start. | integration |
| `S11/stats-pipeline-rollup` | The stats payload's per-pipeline rollup reports latest run state, run counts by state, last exit code, and last run id. | integration |
| `S11/uptime-sole-wall-clock` | Uptime in `iris engine info` is the engine's one and only wall-clock readout and is display-only. | unit |
| `S14/stats-reports-chain-head` | iris engine stats reports the current checkpoint chain head. | integration |
| `S15/engine-info-reports-role` | iris engine info reports whether the queried daemon is currently leader or standby. | integration |

==## E14 Graph Views and Triage Surface==

==**Goal.** The git-graph surface over the two-graphs doctrine (wiring vs lineage, never one rendering): the `iris workload show [<pipeline>]` wiring panel, the `iris run list --graph` lineage rendering with its honesty contract, triage folded into the shows (the gate ledger in `pipeline show`, the blast radius in `deadletter show`, ancestry via `run show --trace [--down]`), the run ref grammar, and the read routes an IDE-style renderer consumes (`/workload`, `/runs?include=inputs`, trace/gate/impact, the `before` cursor).==

==**Depends on.** E05, E07, E09==

==**Cutting tasks.** Cut along: ref grammar plus the gate ledger (the dispatcher's eligibility query re-exposed, code reuse not new logic); blast-radius classification; trace; workload panel; the rail renderer plus its golden files; routes plus the before cursor.==

==**Contracts (16 testable, 1 exempt).**==

==| Contract | Behavior | Tier |==
==| --- | --- | --- |==
==| `S06.2/gate-ledger-in-pipeline-show` | iris pipeline show's gate ledger reports, per depends_on edge, the upstream's latest run, the already-consumed check, and a verdict from the closed set (open, up_to_date, pending, poisoned), and rendering it writes no meta state (a closed gate mints no run row; absence is the record); scripts read the verdict through --json. | integration |==
==| `S06.2/blast-radius-classification` | iris deadletter show walks failed_upstream to the root cause, then classifies each transitive downstream over the wiring plus worklist and run_inputs state as poisoned_now, pending, or shielded, lists composer-only neighbors as untouched, and ends naming the two dispositions (replay, drain). | unit |==
==| `S07/before-reverse-cursor` | Id-keyed collections accept before= as a reverse keyset cursor so log views page newest-first; ordering stays key-driven and no clock enters paging. | unit |==
==| `S07/runs-include-inputs` | GET /runs?include=inputs embeds each run's consumed upstream ids and replayed_from as plain row attributes (parents-per-row, never a separate edge array), streaming as NDJSON like any collection route. | integration |==
==| `S07/trace-gate-impact-routes` | GET /runs/{id}/trace (direction=up or down), GET /pipelines/{name}/gate, and GET /dead_letters/{run_id}/impact serve the same walks the CLI prints, with the verdict and status enums closed. | integration |==
==| `S07/workload-route` | GET /workload serves the wiring payload (lanes, composer order, pipelines with modes and latest run, depends_on edges with gate state) and ?pipeline= zooms to one neighborhood. | integration |==
==| `S08/graph-ascii-golden` | --ascii swaps the rail glyphs for git's own vocabulary, and the --ascii renders are the ones pinned byte-for-byte by golden files. | integration |==
==| `S08/graph-dotted-serial-never-ancestry` | Same-pipeline serial order renders as a dotted rail visually distinct from lineage strokes and creates no edge in any payload. | integration |==
==| `S08/graph-id-gaps-visible` | Missing run ids (deleted queued runs, pruned runs) leave visible gaps; the rendering never renumbers or fabricates continuity. | unit |==
==| `S08/graph-presentational-only` | --graph changes presentation only: the same rows the flat read returns, and --json output never carries drawing. | unit |==
==| `S08/graph-rail-cap-filter-hint` | Past the rail cap the renderer refuses to weave and prints the --lane/--pipeline filter hint instead of degrading. | unit |==
==| `S08/graph-replay-annotation-never-edge` | replayed_from renders as a textual annotation on the run node and never as a graph edge, at the CLI and at the wire alike. | unit |==
==| `S08/graph-solid-edges-run-inputs-only` | In the --graph rendering a solid stroke is drawn for a run_inputs edge and for nothing else. | integration |==
==| `S08/run-ref-grammar` | Everywhere a command accepts <run>, a bare pipeline name resolves to its latest run and <name>~n to the nth prior run, while git's ^ and .. forms are rejected as false cognates. | unit |==
==| `S08/workload-show-wiring-panel` | iris workload show renders the standing wiring (lanes with composer walk, artifact and data mode, run tips, per-edge live gate state) as a panel, never a commit graph, and a named pipeline zooms to that pipeline's neighborhood. | integration |==
==| `S11/two-graphs-never-mixed` | Doctrine: wiring (what may run) and lineage (what did run) are two graphs that never share a rendering; each view cross-references the other by name. | exempt |==
==| `S14/trace-up-down` | iris run show --trace walks run_inputs upward (consumed upstreams) by default and downward with --down (who consumed this run), resolving pruned runs through archival summaries at any depth. | unit |==

## E13 Golden Sample and Acceptance

**Goal.** The golden workspace (two tables, three pipelines in one composed lane, one endpoint) and the 11-step acceptance scenario, each numbered step being the conformance suite for its epic's contracts, plus definition-of-done checks the scenario cannot show: single-file apply idempotence, grants physically enforced, exit codes and `--json` everywhere, cross-compiled static binary boot, the 1.25x capture budget.

**Depends on.** all others (the spine that proves them)

**Cutting tasks.** Starts as soon as E00 exists (fixture workspace is shared), grows one scenario step per landed epic; done last, green unattended.

**Contracts (==31== testable, 0 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| ==`S13/archived-partition-provenance-answers`== | ==iris data provenance== still answers stamps from an archived partition that was exported and dropped from Postgres. | conformance |
| `S13/capture-overhead-bound` | In the bulk-capture benchmark, a 10M-row promoted insert completes within 1.25x of the capture-less baseline. | conformance |
| `S13/checkpoint-chain-validates` | After sealing, the checkpoint chain validates by digest and signature. | conformance |
| `S13/concurrent-writes-commit-order` | Two lanes writing the same row concurrently journal that row's entries in commit order, and ==iris data provenance== names the last committed writer as the current author. | conformance |
| `S13/data-pat-reads-endpoint` | After iris endpoint apply publishes the endpoint, a data PAT minted with --endpoint orders_by_customer reads it via /q/ and via the raw surface. | conformance |
| ==`S13/data-provenance-after-prune`== | After pruning past the writing run, ==iris data provenance== still answers for the row from the archival summary. | conformance |
| ==`S13/data-provenance-full-lineage`== | ==iris data provenance== analytics.orders <pk> answers with the writing run, pipeline, hashes, and consumed upstream in one query. | conformance |
| `S13/dev-run-rows-journaled` | A dev/disposable lane run drives all three pipelines, lands rows in the real tables, and records those rows in the data journal. | conformance |
| `S13/failover-standby-takeover` | When the leader is killed, a second candidate on the same meta acquires the lock, reconciles (orphan dead-letters marked stopped), reports itself leader, and resumes lanes. | conformance |
| `S13/failure-propagates-composer-runs` | A forced failure in extract_orders dead-letters it and propagates to load_orders via depends_on, while reset_counters (composer-only ordering) still runs. | conformance |
| `S13/four-applies-register-graph` | Four single-file iris declare apply invocations (ingest composer first, then extract_orders, reset_counters, load_orders) register the full sample graph, with schema provisioning riding each apply. | conformance |
| `S13/idle-lane-chains-noop-passes` | An idle lane keeps chaining passes (the pass counter climbs) and every no-op run exits 0 cheaply. | conformance |
| `S13/install-creates-meta-and-data` | iris engine install creates the meta database alongside the data database. | conformance |
| `S13/install-start-one-codepath` | iris engine install plus iris engine start brings up managed Postgres locally and external mode against a CI service container through one code path. | conformance |
| `S13/per-pipeline-watermark` | Each of the three sample pipelines advances its own watermark independently across runs. | conformance |
| `S13/promoted-writes-wipe-immune` | After iris pipeline build and promote, a re-run's writes are still captured in the journal but are not wipe-eligible, and a subsequent wipe leaves them intact. | conformance |
==| `S13/blast-radius-readout` | Before replay, iris dl show on the propagated entry walks to the root cause and names load_orders poisoned and reset_counters untouched (order is not dependency). | conformance |==
| `S13/replay-root-walk-supersedes` | iris deadletter replay auto-walks to the root failure, clears the worklist, and discards the propagated entry as superseded. | conformance |
| `S13/run-cancel-lane-proceeds` | iris run cancel ends a hung pipeline, dead-letters it as stopped, and the lane proceeds past it. | conformance |
| `S13/sample-dependency-split` | In the sample declarations, load_orders declares depends_on [extract_orders] and reset_counters declares no depends_on; walk position for all three comes from composer order alone, so the lane sequence is independent of data dependencies. | unit |
| `S13/sample-workspace-shape` | The golden sample workspace parses to exactly two tables (raw.orders_staging, analytics.orders), three single-script pipelines in one ingest lane composed by ingest/iris-declare.yaml with order [extract_orders, reset_counters, load_orders], and one declared read endpoint orders_by_customer, with extract_orders reading and writing raw.orders_staging and load_orders reading staging and writing analytics.orders. | unit |
| `S13/scenario-passes-unattended` | The golden sample passes the full 11-step acceptance scenario end-to-end unattended. | conformance |
| `S13/seal-compaction-drops-consumed` | Sealing compacts the partition: released pre-images are nulled and duplicate stamps are folded. | conformance |
| `S13/seal-waits-for-inflight-run` | A journal partition driven past its size threshold while a run is in flight is not cut mid-run; sealing waits for that run to finish so the journal window stays whole. | conformance |
| `S13/sealed-partition-exports-drops` | A sealed partition exports to the object store and drops from Postgres. | conformance |
| `S13/standby-mutation-exit-6` | A mutation attempted against a standby is rejected with leader guidance and exit code 6. | conformance |
| `S13/static-cross-compile-boot` | The engine cross-compiles to a single static binary that boots with no host runtime installed. | conformance |
| `S13/ungranted-field-fails-postgres` | A read touching an ungranted field fails at Postgres itself rather than in application code. | conformance |
| `S13/wipe-reverts-dev-run` | ==iris workload wipe== after the dev/disposable run reverts the landed rows, proving the journal drives the wipe. | conformance |
==| `S13/scoped-wipe-single-pipeline` | A named iris workload wipe extract_orders reverts only that pipeline's writes, and the following bare iris workload wipe reverts the rest. | conformance |==
| `S16/cross-compile-smoke` | Cross-compiled binaries for linux and macos on amd64 and arm64 each boot, install, start, apply, and tear down successfully in the smoke run. | conformance |

## E15 Onboarding and Guided Tour

**Goal.** The installer's handoff: `iris quickstart`, a third root verb that tutors the first session — explains, confirms, then really runs `engine install`, `engine start -d`, `engine info`, materializes the embedded `hello_iris` sample (seven rainbow colors into `demo.colors`), applies and runs it, and ends on `iris data provenance demo.colors green` with the engine left running and a printed cheat-sheet. Interactive only on a real terminal (stdin and stdout TTY, `--json` off); otherwise a plain numbered copy-paste guide — or a `--json` step-list envelope — that executes nothing. Every step executes the real command implementation through the tour's own binary, so the tour can never do what the commands cannot; every step is the command's own idempotence, so abort and resume are free. `install.sh` ends by offering the tour over `/dev/tty`.

**Depends on.** E02 (engine lifecycle), E03 (declare), E05 (runs), E07 (provenance) — the surfaces it tours.

**Cutting tasks.** Two seams: first the surface (verb, gates, three renderings, embedded sample, the `startDetached` argv refactor), then the orchestration (sequencer, prompts, adaptive skip, `--yes`, installer handoff, conformance leg).

**Contracts (13 testable, 1 exempt).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S08/quickstart-root-verb` | `iris quickstart` is the third root verb beside update/uninstall; the tree stays nine nouns + three root verbs; bare invocation is valid (not a group stub) and daemonless. | unit |
| `S08/quickstart-tty-gating` | The interactive tour runs only when stdin and stdout are both interactive terminals and `--json` is off; any other invocation gets the plain guide. | unit |
| `S08/quickstart-ceremony-color-gating` | NO_COLOR strips ANSI from the tour but never disables interactivity; piped or `--json` output never carries an escape. | unit |
| `S08/quickstart-sample-valid-declaration` | The embedded sample declaration and table file parse through the real declare loaders (name matches folder, explicit fields, no lane). | unit |
| `S08/quickstart-refuses-remote-host` | `--host` on quickstart is a usage error (exit 2) with local-tour guidance; `--socket` stays accepted. | unit |
| `S08/quickstart-plain-guide-when-piped` | A non-TTY invocation prints the complete numbered copy-paste guide, byte-stable plain text, executes nothing, exits 0. | integration |
| `S08/quickstart-json-guide-envelope` | `--json` emits one data envelope carrying the ordered step list (id, explanation, argv) and executes nothing. | integration |
| `S08/quickstart-step-order-confirmed` | Steps execute in tour order (install, start -d, info, apply, run, provenance), each only after an affirmative prompt, via the tour's own binary, never a PATH lookup. | integration |
| `S08/quickstart-decline-clean-abort` | Declining any step (or EOF/interrupt) exits 0 with a resume hint; nothing past the decline executes. | integration |
| `S08/quickstart-adaptive-skip-running-engine` | A reachable daemon on the workspace socket announces install/start as already done and skips them; the tour proceeds from the info step. | integration |
| `S08/quickstart-sample-materialize-never-clobber` | Sample files are written only when absent, byte-identical to the embedded golden; a present-but-different file is kept and warned about. | integration |
| `S08/quickstart-yes-runs-unattended` | `--yes` runs every step without prompting, works piped without ANSI, and exits with the first failing step's category. | integration |
| `S08/quickstart-full-tour` | Real binary + real Postgres: `quickstart --yes` in a fresh workspace bootstraps the engine, applies and runs the sample, and `iris data provenance demo.colors green` names the run; the engine is left running; a second run exits 0. | conformance |
| `S08/quickstart-install-handoff` | install.sh handoff prose: /dev/tty Y/n prompt (default yes), exec of the absolute-path binary with stdin re-tied, IRIS_FORCE/no-terminal fallback next-steps lines. | exempt |

## E16 Install Ceremony and Pipeline Catalog

**Goal.** `curl … | sh` becomes the guide itself: a chaptered ceremony — THE CLI (install.sh: banner, download, checksum, staged in the update grammar), THE ENGINE (workspace question defaulting `~/iris`, `engine install`, `engine start -d`, readout), THE PIPELINE (browse the embedded starter catalog, pick one, materialize/apply/run it, close on its provenance showcase). Consent per act, not per step; chapters named, never numbered, marked by the light rule-and-title device riding the rainbow palette. install.sh stays thin and version-gates the handoff by probing the installed binary (`quickstart --from-installer --json`), so an old release binary is never offered a verb it lacks. The catalog is embedded (go:embed, air-gapped), one folder per entry (`entry.yaml` metadata + `workspace/` subtree), ordered by `catalog.yaml`, default `hello_iris`; `--pipeline <id>` picks explicitly everywhere; every entry parses through the real declare loaders by test.

**Depends on.** E15 (the tour it restructures).

**Cutting tasks.** Four seams: the act framework and workspace prompt (carries the whole §8 delta), the catalog registry and entries, the shop and picked tour, the installer restaging and version gate.

**Contracts (9 testable, 1 exempt; plus retargeted E15 rows noted in the task briefs).**

| Contract | Behavior | Tier |
| --- | --- | --- |
| `S08/quickstart-act-structure` | Chaptered tour: ENGINE then PIPELINE chapter marks (TTY-only), steps grouped and ordered within acts, consent per act, first failing step stops the tour with that command's exit category. | integration |
| `S08/quickstart-workspace-prompt` | The ENGINE act opens `Engine workspace [~/iris]:` — empty answer accepts, `~` expands, mkdir -p + chdir; a cwd that is already a workspace is proposed back as the default; `--yes` never prompts and uses cwd. | integration |
| `S08/quickstart-from-installer-continuation` | `--from-installer` opens directly on the ENGINE chapter with no welcome or act gate (the installer's Y/n was consent); combined with `--json` it stays the inert step-list envelope, exit 0 — the version-probe guarantee. | integration |
| `S08/install-ceremony-version-gate` | install.sh probes the installed binary (`quickstart --from-installer --json`, else plain `quickstart --json`) and hands off accordingly; anything older gets no handoff and no quickstart next-step line. | exempt |
| `S08/quickstart-catalog-entries-valid` | Every embedded catalog entry parses through the real declare loaders; `entry.yaml` id matches its folder, fields non-empty, showcase.table among the entry's declared writes; `catalog.yaml` lists exactly the entry folders, ids unique, `hello_iris` first. | unit |
| `S08/quickstart-catalog-browse-render` | The pipeline act paints the numbered shop (name + pitch), prompts `Pick a pipeline (1-N, Enter=1):`, empty answer takes entry 1, picked entry's description + finale preview render before the apply confirm; non-numeric or out-of-range answer aborts clean. | integration |
| `S08/quickstart-catalog-pick-materialize-run` | Picking a non-default entry drives the act with that entry only: just its files materialize, apply/run/provenance argvs carry its id and showcase, the dead-letter lesson names it. | integration |
| `S08/quickstart-catalog-pipeline-flag` | `--pipeline <id>` selects the entry explicitly in every rendering; an unknown id is a usage error (exit 2) naming the available ids. | integration |
| `S08/quickstart-catalog-in-guides` | The plain guide and `--json` envelope carry the catalog (ids, pitches, default and selected) plus the selected entry's steps, executing nothing. | integration |
| `S08/quickstart-catalog-picked-full-tour` | Real binary + real Postgres: `quickstart --yes --pipeline word_frequency` in a fresh workspace bootstraps, applies, runs, and `iris data provenance demo.word_counts hope` names the run; re-running exits 0. | conformance |
