---
status: draft
created: 2026-07-04
modified: 2026-07-14
tags:
  - iris
  - epics
  - planning
---

# Iris Epics

Sixteen capability epics (E01 to E16) in build-dependency order. This file is the standing map of the engine: what each epic is for, what it depends on, where its natural task seams lie, and — first — the doctrine every epic is built to obey. Read the doctrine before changing engine behavior; it is the part that is expensive to rediscover.

## Engine doctrine

These are the load-bearing invariants. They are not preferences and they are not negotiable inside a task: a change that breaks one of them is a change to what Iris is, and belongs in a conversation, not a commit.

- **No scheduler.** Orchestration is engine-owned. Scripts never call each other and never call the engine, and no scheduler, cron, timer, or reactive trigger exists anywhere in the system. The dependency allowlist bans scheduler libraries outright, and `internal/arch` enforces that ban.
- **No clocks.** There is no time-based operation anywhere. Clocks never initiate, order, gate, bound, or end work: ordering keys are monotonic bigint identities, retention is count-based, log rotation is size-based, and a run ends only by exiting on its own or by explicit `iris run cancel` — never on elapsed time. The single wall-clock readout in the whole engine is the display-only uptime line in `iris engine info`. `recorded_at` columns exist for log correlation and are opaque strings, never an ordering key. The hot idle loop is the accepted price of this, kept cheap with watermarks.
- **State is data.** Pipeline state — cursors, offsets, high-water keys — lives in a declared table that the pipeline itself writes. It never lives in an engine field, a declaration slot, a `pipelines` column, or a meta key-value store. The engine ships no row deltas to pipelines; a pipeline reads its own state back out of its own table. This is why wiping a disposable run rolls its cursor advance back together with its data.
- **Row safety is Postgres.** Concurrent-write row safety, current-value semantics (last committed writer wins), and read-modify-write atomicity all come from ordinary Postgres MVCC, row locks, and the pipeline's own transaction. The engine adds no concurrency mechanism of its own and imposes no global cross-row order.
- **Table ownership is split and absolute.** The `schemas/` tree is the sole truth for declared tables. The engine owns the journal, the capture triggers, and meta as undeclared, non-user-editable surfaces. Neither side edits the other's: an engine-added column on a user table is non-additive drift and is refused, and capture triggers and the journal are excluded from drift comparison entirely.
- **One store, not two.** Disposable and permanent rows live in the same declared tables, with no separate schema, copy, or namespace, and no seeding or mirroring. Dev pipelines share upstream tables via `depends_on` exactly as production does. Isolation is logical, not physical: a row change is disposable exactly while its journal entries remain in wipe scope. Permanent rows derived from since-wiped inputs are the author's to reconcile.
- **Two graphs, never mixed.** Wiring (what may run — lanes, composer order, `depends_on` edges) and lineage (what did run — runs and the `run_inputs` they consumed) are two different graphs, and they never share a rendering. `iris workload show` paints wiring as a panel; `iris run list --graph` paints lineage as a rail. Each view cross-references the other by name and neither pretends to be the other.
- **Reads are not captured.** Write triggers capture writes only. Read-set provenance is out of scope: it is a north star, not a behavior, and nothing in the engine should be built as if it were coming.
- **No scale-out.** Runs execute only as subprocesses on the leader's host. The scaling levers are lanes and host capacity; remote execution is out of scope by design. The remaining availability and scale ceiling is Postgres itself, and the engine is deliberately indifferent to where `pg_dsn` points (managed local, RDS, Aurora, Citus): meta availability and data-plane scale are bought with a better Postgres, not with engine work. Managed Postgres is a daemon subprocess and dies with its host, so availability beyond one host means external mode.
- **External events are out of scope.** Files, webhooks, and queues are user code at the boundary, not an engine concern. The engine stays a pure loop plus dependency gate.

## How to work an epic

- A task is a small coherent cluster of behavior inside one epic; the per-epic cutting notes below suggest the seams.
- Tests come first and they carry the design: write the failing test at the cheapest tier that can actually prove the behavior, implement to green, keep the rest of the suite green.
- Tier legend, by execution cost — unit: pure logic, no I/O. Integration: fakes and local process I/O, no live Postgres. Conformance: the real shipped binary against a running daemon and a real Postgres.
- An epic is done when its behavior is proven green at the right tiers and the golden sample scenario still passes.

## Test harness

The harness is a permanent asset and predates every epic below; it is not itself an epic.

- **Golden files.** Every generated artifact — SQL and DDL, migration YAML, `--dry-run` previews, `--json` output, graph renders — is compared byte-for-byte against a checked-in golden file, and any difference fails. Running with the `-update` flag regenerates the goldens in place instead of failing. A golden diff is a behavior diff: it must be looked at, never blindly regenerated.
- **Fixtures.** The golden workspace (the sample project of E13), plus valid and deliberately invalid declaration fixtures.
- **Fakes at the seams.** `store`, `pg`, and `exec` each sit behind an interface satisfied by a fake: a recording `pg` fake captures the exact CREATE/ALTER/GRANT and trigger DDL as golden files, a meta-store fake stands in for meta, and a fake process runner lets dispatch tests start runs in composer order, stream output, and cancel a run mid-flight with no real process and no database.
- **Real I/O where it is cheap.** Process I/O that needs no database — subprocess execution, output capture, cancel and kill, socket HTTP against an in-process daemon — is tested for real against throwaway scripts rather than faked.
- **Conformance runner.** Conformance tests drive the actual shipped binary against a running daemon over the socket, asserting exit codes and `--json` output, with a real Postgres created by the engine itself hosting both databases. It is the only tier where generated SQL meets a live database.
- **No fixed sleeps.** Failover and other async assertions wait on connection and run state. A fixed sleep in a test is a bug.
- **CI is the merge gate.** Per push and PR: golangci-lint, database-free unit plus integration on the Go version matrix, `go build`, and conformance against a Postgres service container (admin role carrying CREATEDB, failover leg included). Green across all stages is the merge gate. Windows is deferred from v1 — unix socket, service wrapper, and process-group kill each need Windows-specific answers — and is revisited post-v1.

## Epic map

| Epic | Name                                           | Depends on                                                  |
| ---- | ---------------------------------------------- | ----------------------------------------------------------- |
| E01  | Repo Skeleton, CLI Frame and Config            | none                                                        |
| E02  | Engine Install, Daemon and Leadership          | E01                                                         |
| E03  | Declarations, Schemas and Apply                | E01 for pure parsing, E02 for apply against the daemon      |
| E04  | Roles, Grants and Credentials                  | E02, E03                                                    |
| E05  | Dispatcher, Lanes and Dead Letters             | E02, E03                                                    |
| E06  | Write Capture, Wipe and Promotion              | E03 (provisioning installs triggers), E05 (run attribution) |
| E07  | Provenance, Journal Lifecycle and Object Store | E05, E06                                                    |
| E08  | Build, Artifacts and Modes                     | E03, E05                                                    |
| E09  | Read API, Endpoints and PATs                   | E02 (server), E03 (schemas), E04 (roles)                    |
| E10  | Destructive Operation Gates                    | E03, E05, E06 (the operations it gates)                     |
| E11  | High Availability and Failover                 | E02, E05                                                    |
| E12  | Stats, Info and Inspect                        | E02, E05                                                    |
| E13  | Golden Sample and Acceptance                   | all others (the spine that proves them)                     |
| E14  | Graph Views and Triage Surface                 | E05, E07, E09                                               |
| E15  | Onboarding and Guided Tour                     | E02, E03, E05, E07 (the surfaces it tours)                  |
| E16  | Install Ceremony and Pipeline Catalog          | E15 (the tour it restructures)                              |

Build order is the table order with one exception: E14 builds before E13, the acceptance spine that proves everything above it, E14 included. The epic sections below are written in build order, so E14 appears before E13.

## E01 Repo Skeleton, CLI Frame and Config

**Goal.** One Go module, application layout under `internal/`, `cmd/iris` entrypoint, the full cobra noun-verb tree as stubs, global flags, exit-code categories, the `--json` output convention, config precedence (flags over env over `iris.toml` over defaults), lint and CI wiring, Go 1.25/1.26 matrix.

**Depends on.** none

**Cutting tasks.** Cut tasks along: module and package layout; cobra tree plus exit codes; config precedence; CI plus lint. Stubs return `not implemented`, but exit codes and `--json` envelope are real from day one.

## E02 Engine Install, Daemon and Leadership

**Goal.** Everything between `iris engine install` and a leader dispatching: meta bootstrap DDL (17 tables, create-if-missing), managed and external Postgres on one code path, the admin-DSN resolution chain, unix socket plus opt-in TCP/TLS listeners, daemon foreground/detach lifecycle, signal handling, leader election on the advisory lock, the single-writer meta path, crash reconciliation, log rotation, service-unit install.

**Depends on.** E01

**Cutting tasks.** Cut tasks along: install/uninstall plus meta DDL; managed Postgres subprocess; DSN chain plus fail-fast guards; listeners plus CLI-daemon protocol; leader election plus single-writer store; crash reconciliation; logging plus service unit.

## E03 Declarations, Schemas and Apply

**Goal.** The declared world: parse and validate `iris-declare.yaml` (eight fields, nothing else), lane composer rules and the 2+ interlock, `declare apply`/`destroy` single-file semantics with upstream-first registration and acyclicity, the `schemas/` tree, the closed YAML-to-Postgres type mapping, idempotent provisioning, additive-only migrations, schema and ledger drift detection.

**Depends on.** E01 for pure parsing, E02 for apply against the daemon

**Cutting tasks.** Parsing, validation, diffing and drift classification are pure unit logic and can start before E02 lands; apply/destroy against a live daemon closes the epic. The recording `pg` fake with golden DDL files carries most integration weight.

## E04 Roles, Grants and Credentials

**Goal.** Least-privilege plumbing: one engine-owned Postgres role per pipeline, the grants ledger in meta reconciled onto the data database, engine-managed credentials, scoped connection injection at spawn, grant drift detection, the reserved `public` schema bounds.

**Depends on.** E02, E03

**Cutting tasks.** Small and coherent; likely three tasks: role plus credential lifecycle; grant reconcile plus drift; connection injection.

## E05 Dispatcher, Lanes and Dead Letters

**Goal.** The engine's heart: subprocess execution (process groups, output capture, cancel and kill), run records and states, perpetual lane runners walking composer order, the `depends_on` gate with `run_inputs` consumption, lazy failure propagation, the dead-letter worklist with replay and drain, count-based retention with pruning into `run_summaries`, manual `pipeline run`. This is where the no-scheduler, no-clock and state-as-data doctrines are physically enforced.

**Depends on.** E02, E03

**Cutting tasks.** Biggest epic; cut along its natural seams: exec seam; run records plus states; lane loop plus pass semantics; gate plus consumption; propagation; dead-letter lifecycle (replay root-walk, drain, supersession); retention plus pruning plus summaries; manual run. Gate and propagation logic is pure unit work against the fake process runner.

## E06 Write Capture, Wipe and Promotion

**Goal.** Always-on provenance capture: the partitioned `data_journal`, statement-level triggers with transition tables, run attribution riding the injected connection, stamp versus pre-image payload tiers, `iris workload wipe [<pipeline>]` reverse replay with the journal-internal conflict rule and the wiped/skipped disposition split, promotion flipping wipe eligibility, the data-durability gate.

**Depends on.** E03 (provisioning installs triggers), E05 (run attribution)

**Cutting tasks.** Cut tasks along: journal DDL plus partitioning; trigger emission (golden PL/pgSQL); attribution; wipe replay plus conflict rule (pure unit on journal fixtures); promotion. The 1.25x bulk-capture budget lands in E13, not here.

## E07 Provenance, Journal Lifecycle and Object Store

**Goal.** The read side of capture and its long-term shape: the `iris data provenance` three-lookup walk, the snapshot pin, seal plus compaction (null released pre-images, fold duplicate stamps), the signed checkpoint chain, the content-addressed object store, partition export and archived reads, archival summaries keeping lineage whole after pruning.

**Depends on.** E05, E06

**Cutting tasks.** Cut tasks along: provenance walk; pin; seal condition plus compaction; checkpoint chain plus engine key; object store plus export; archived reads. Digest chains and compaction collapse are pure unit logic; the archive format round-trips against temp files.

## E08 Build, Artifacts and Modes

**Goal.** Artifact production and the mode matrix: one pinned recipe per language (Go, Python, Node), content addressing into `artifacts` plus object-store bytes, `iris pipeline build` and `promote` gating, the artifact/data mode matrix (source+permanent blocked), artifact retirement.

**Depends on.** E03, E05

**Cutting tasks.** Recipe inference and matrix validation are unit work; actual PyInstaller/pkg builds are conformance-tier and can ride E13.

## E09 Read API, Endpoints and PATs

**Goal.** The network surface: PAT mint/verify with argon2id and show-once, scope checks per route, data-PAT NOLOGIN roles assumed via SET ROLE on a shared read pool, the full GET route set, endpoint YAML lifecycle with deterministic SQL compilation and prepare-verification, the wire contract (param grammar, envelope, closed error codes, keyset pagination, per-type serialization), NDJSON streaming.

**Depends on.** E02 (server), E03 (schemas), E04 (roles)

**Cutting tasks.** Cut tasks along: PAT store plus scopes; route mux plus auth split; endpoint compile plus lifecycle; /q and /data execution path; wire contract (largest test surface, mostly unit against the compiled shapes plus socket HTTP integration); NDJSON.

## E10 Destructive Operation Gates

**Goal.** The five gated destructive operations, tiered confirmation (type-the-name versus y/N), soft-blocks with `--yes`/`--force` semantics, destroy blocker rules (dependents, `run_inputs` references, dead-letter references), remote-surface tiering (uninstall never remote), failover never resuming an interrupted destructive op.

**Depends on.** E03, E05, E06 (the operations it gates)

**Cutting tasks.** Gate and blocker predicates are unit logic; the confirmations and remote tiering close at conformance.

## E11 High Availability and Failover

**Goal.** Availability of the one meta: standby candidates blocking on the advisory lock, promotion running the same reconciliation as restart, explicit self-demotion on session loss (stop dispatching, kill in-flight), workspace-tree and object-store per-host prerequisites, standby mutation rejection with leader guidance.

**Depends on.** E02, E05

**Cutting tasks.** Leader-lock logic against the meta-store fake; one real conformance leg (two candidates, kill the leader) rides E13 step 9.

## E12 Stats, Info and Inspect

**Goal.** Read-only readouts: `iris engine stats` rollups (identical over CLI and GET /stats), `info` (role, listeners, key, uptime as the sole wall-clock), `inspect` DDL dump, clock-free counters, no metrics endpoint in core.

**Depends on.** E02, E05

**Cutting tasks.** Small; one or two tasks once their source data exists.

## E14 Graph Views and Triage Surface

**Goal.** The git-graph surface over the two-graphs doctrine (wiring vs lineage, never one rendering): the `iris workload show [<pipeline>]` wiring panel, the `iris run list --graph` lineage rendering with its honesty contract (solid strokes for `run_inputs` edges and nothing else, dotted rails for same-pipeline serial order, visible gaps for missing run ids, `replayed_from` as annotation and never an edge, a filter hint past the rail cap rather than a degraded weave), triage folded into the shows (the gate ledger in `pipeline show`, the blast radius in `deadletter show`, ancestry via `run show --trace [--down]`), the run ref grammar, and the read routes an IDE-style renderer consumes (`/workload`, `/runs?include=inputs`, trace/gate/impact, the `before` cursor).

**Depends on.** E05, E07, E09

**Cutting tasks.** Cut along: ref grammar plus the gate ledger (the dispatcher's eligibility query re-exposed, code reuse not new logic); blast-radius classification; trace; workload panel; the rail renderer plus its golden files; routes plus the before cursor.

## E13 Golden Sample and Acceptance

**Goal.** The golden workspace (two tables, three pipelines in one composed lane, one endpoint) and the 11-step acceptance scenario, each numbered step being the conformance leg for the epic it proves, so "the sample passes" and "the engine is correct" are one statement. Plus the definition-of-done checks the scenario cannot show: single-file apply idempotence, grants physically enforced, exit codes and `--json` everywhere, cross-compiled static binary boot, the 1.25x capture budget.

**Depends on.** all others (the spine that proves them)

**Cutting tasks.** Starts as soon as the harness exists (the fixture workspace is shared), grows one scenario step per landed epic; done last, green unattended.

## E15 Onboarding and Guided Tour

**Retired.** The `iris quickstart` verb, its tour, and the installer handoff were removed in #210: `curl … | bash` is the only install route, ending on plain next-steps lines. This epic stands as the historical record of what shipped and was later cut.

**Goal.** The installer's handoff: `iris quickstart`, a third root verb that tutors the first session — explains, confirms, then really runs `engine install`, `engine start -d`, `engine info`, materializes the embedded `hello_iris` sample (seven rainbow colors into `demo.colors`), applies and runs it, and ends on `iris data provenance demo.colors green` with the engine left running and a printed cheat-sheet. Interactive only on a real terminal (stdin and stdout TTY, `--json` off); otherwise a plain numbered copy-paste guide — or a `--json` step-list envelope — that executes nothing. Every step executes the real command implementation through the tour's own binary, so the tour can never do what the commands cannot; every step is the command's own idempotence, so abort and resume are free. `install.sh` ends by offering the tour over `/dev/tty`.

**Depends on.** E02 (engine lifecycle), E03 (declare), E05 (runs), E07 (provenance) — the surfaces it tours.

**Cutting tasks.** Two seams: first the surface (verb, gates, three renderings, embedded sample, the `startDetached` argv refactor), then the orchestration (sequencer, prompts, adaptive skip, `--yes`, installer handoff, conformance leg).

## E16 Install Ceremony and Pipeline Catalog

**Retired.** Removed with E15 in #210: the chaptered ceremony, the embedded pipeline catalog, and install.sh's version-gated handoff are gone; install.sh downloads, verifies, installs, and prints the next-steps line.

**Goal.** `curl … | sh` becomes the guide itself: a chaptered ceremony — THE CLI (install.sh: banner, download, checksum, staged in the update grammar), THE ENGINE (workspace question defaulting `~/.iris/workspace`, `engine install`, `engine start -d`, readout), THE PIPELINE (browse the embedded starter catalog, pick one, materialize/apply/run it, close on its provenance showcase). Consent per act, not per step; chapters named, never numbered, marked by the light rule-and-title device riding the rainbow palette. install.sh stays thin and version-gates the handoff by probing the installed binary (`quickstart --from-installer --json`), so an old release binary is never offered a verb it lacks. The catalog is embedded (go:embed, air-gapped), one folder per entry (`entry.yaml` metadata + `workspace/` subtree), ordered by `catalog.yaml`, default `hello_iris`; `--pipeline <id>` picks explicitly everywhere; every entry parses through the real declare loaders by test.

**Depends on.** E15 (the tour it restructures).

**Cutting tasks.** Four seams: the act framework and workspace prompt, the catalog registry and entries, the shop and picked tour, the installer restaging and version gate.
