# BUILD_STATE

Orchestrator resume file. One line per task: status ∈ {todo, in-progress, done}; done
lines carry the PR that merged the task's main work into `development`. Epic rows track
the development→master checkpoint PR. Task briefs live in `docs/Tasks/`. Cross-cutting
devfix/debt PRs are listed once at the bottom.

**Status 2026-07-10: all epics complete on development.** Traceability backlog: 0
unclaimed non-exempt contracts; strict gate passes (definition of done met). Full CI —
build matrix, unit+integration (Go 1.25/1.26), golangci-lint, traceability gate, full
conformance suite (real binary, real Postgres, -race) — green.

## Open items

- **E15 Onboarding and Guided Tour: complete on development 2026-07-11** (PRs #133,
  #134; 14 S08/quickstart-* contracts, strict gate zero unclaimed). Epic checkpoint
  PR to master not opened — joins the E04–E14 checkpoint queue awaiting human review.
- **Epic checkpoint PRs to master**: none opened yet for E04–E14/E13; await human review
  per branching rules.
- **External conformance clusters must be PostgreSQL 16+** (`INHERIT FALSE` grant
  syntax); PG14 fails with 42601. CI's postgres:17 satisfies this.
- **`GET /runs` cursor params**: route accepts only `include`; `before`/`after` paging
  on this route is unimplemented (S07/before-reverse-cursor is claimed at its unit tier
  on the params planner; `iris run list` does not send cursors). Noted in PR #118.
- **Standby endpoint propagation**: a standby reloads applied endpoints from meta on
  its own start; live cross-node propagation of a leader's later applies to a running
  standby is out of scope of PR #114.

## E00 Conformance Harness and Traceability Gate — epic PR [#6](https://github.com/MateusAMP2119/iris-engine-cli/pull/6) (merged to master)

- [x] E00.1 Manifest seed and doctrine — done ([PR #1](https://github.com/MateusAMP2119/iris-engine-cli/pull/1))
- [x] E00.2 Traceability gate — done ([PR #2](https://github.com/MateusAMP2119/iris-engine-cli/pull/2))
- [x] E00.3 Golden files and fixtures — done ([PR #3](https://github.com/MateusAMP2119/iris-engine-cli/pull/3))
- [x] E00.4 Fakes and process IO — done ([PR #5](https://github.com/MateusAMP2119/iris-engine-cli/pull/5))
- [x] E00.5 Conformance runner and CI — done ([PR #4](https://github.com/MateusAMP2119/iris-engine-cli/pull/4))

## E01 Repo Skeleton, CLI Frame and Config — epic PR [#14](https://github.com/MateusAMP2119/iris-engine-cli/pull/14) (merged to master)

- [x] E01.1 Module and package layout — done ([PR #10](https://github.com/MateusAMP2119/iris-engine-cli/pull/10))
- [x] E01.2 Cobra tree and exit codes — done ([PR #12](https://github.com/MateusAMP2119/iris-engine-cli/pull/12))
- [x] E01.3 Config precedence — done ([PR #13](https://github.com/MateusAMP2119/iris-engine-cli/pull/13))
- [x] E01.4 CI and lint wiring — done ([PR #11](https://github.com/MateusAMP2119/iris-engine-cli/pull/11))

## E02 Engine Install, Daemon and Leadership — epic PR [#23](https://github.com/MateusAMP2119/iris-engine-cli/pull/23) (merged to master)

- [x] E02.1 Meta DDL and schema — done ([PR #15](https://github.com/MateusAMP2119/iris-engine-cli/pull/15))
- [x] E02.2 Admin DSN chain — done ([PR #16](https://github.com/MateusAMP2119/iris-engine-cli/pull/16))
- [x] E02.3 Managed Postgres subprocess — done ([PR #17](https://github.com/MateusAMP2119/iris-engine-cli/pull/17))
- [x] E02.4 Install and uninstall — done ([PR #18](https://github.com/MateusAMP2119/iris-engine-cli/pull/18))
- [x] E02.5 Listeners and daemon protocol — done ([PR #19](https://github.com/MateusAMP2119/iris-engine-cli/pull/19))
- [x] E02.6 Leader election single writer — done ([PR #20](https://github.com/MateusAMP2119/iris-engine-cli/pull/20))
- [x] E02.7 Crash reconciliation — done ([PR #22](https://github.com/MateusAMP2119/iris-engine-cli/pull/22))
- [x] E02.8 Logging and service unit — done ([PR #21](https://github.com/MateusAMP2119/iris-engine-cli/pull/21))

## E03 Declarations, Schemas and Apply — epic PR [#36](https://github.com/MateusAMP2119/iris-engine-cli/pull/36) (merged to master)

- [x] E03.1 Declaration parsing and discovery — done ([PR #24](https://github.com/MateusAMP2119/iris-engine-cli/pull/24))
- [x] E03.2 Lane composer validation — done ([PR #27](https://github.com/MateusAMP2119/iris-engine-cli/pull/27))
- [x] E03.3 Single file targets — done ([PR #26](https://github.com/MateusAMP2119/iris-engine-cli/pull/26))
- [x] E03.4 Dependency graph validation — done ([PR #25](https://github.com/MateusAMP2119/iris-engine-cli/pull/25))
- [x] E03.5 Type mapping and DDL — done ([PR #28](https://github.com/MateusAMP2119/iris-engine-cli/pull/28))
- [x] E03.6 Drift classification — done ([PR #30](https://github.com/MateusAMP2119/iris-engine-cli/pull/30))
- [x] E03.7 Migration ledger sync — done ([PR #31](https://github.com/MateusAMP2119/iris-engine-cli/pull/31))
- [x] E03.8 Idempotent provisioning — done ([PR #32](https://github.com/MateusAMP2119/iris-engine-cli/pull/32))
- [x] E03.9 Registry persistence in meta — done ([PR #29](https://github.com/MateusAMP2119/iris-engine-cli/pull/29))
- [x] E03.10 Apply destroy closure — done ([PR #35](https://github.com/MateusAMP2119/iris-engine-cli/pull/35))

## E04 Roles, Grants and Credentials — epic PR: pending

- [x] E04.1 Access declaration validation — done ([PR #34](https://github.com/MateusAMP2119/iris-engine-cli/pull/34))
- [x] E04.2 Role and credential lifecycle — done ([PR #37](https://github.com/MateusAMP2119/iris-engine-cli/pull/37))
- [x] E04.3 Grant reconcile and drift — done ([PR #43](https://github.com/MateusAMP2119/iris-engine-cli/pull/43))
- [x] E04.4 Connection injection and enforcement — done ([PR #46](https://github.com/MateusAMP2119/iris-engine-cli/pull/46))

## E05 Dispatcher, Lanes and Dead Letters — epic PR: pending

- [x] E05.1 Exec seam — done ([PR #39](https://github.com/MateusAMP2119/iris-engine-cli/pull/39))
- [x] E05.2 Run environment — done ([PR #40](https://github.com/MateusAMP2119/iris-engine-cli/pull/40))
- [x] E05.3 Run records and states — done ([PR #42](https://github.com/MateusAMP2119/iris-engine-cli/pull/42))
- [x] E05.4 Lane model and walk — done ([PR #38](https://github.com/MateusAMP2119/iris-engine-cli/pull/38))
- [x] E05.5 Gate and consumption — done ([PR #44](https://github.com/MateusAMP2119/iris-engine-cli/pull/44))
- [x] E05.6 Failure propagation — done ([PR #45](https://github.com/MateusAMP2119/iris-engine-cli/pull/45))
- [x] E05.7 Dead letter replay — done ([PR #47](https://github.com/MateusAMP2119/iris-engine-cli/pull/47))
- [x] E05.8 Dead letter drain — done ([PR #50](https://github.com/MateusAMP2119/iris-engine-cli/pull/50))
- [x] E05.9 Retention and pruning — done ([PR #54](https://github.com/MateusAMP2119/iris-engine-cli/pull/54))
- [x] E05.10 Manual pipeline run — done ([PR #49](https://github.com/MateusAMP2119/iris-engine-cli/pull/49))
- [x] E05.11 Doctrines and scope — done ([PR #88](https://github.com/MateusAMP2119/iris-engine-cli/pull/88))
- [x] E05.12 Lane runner pass semantics — done ([PR #59](https://github.com/MateusAMP2119/iris-engine-cli/pull/59))

## E06 Write Capture, Wipe and Promotion — epic PR: pending

- [x] E06.1 Journal DDL and partitioning — done ([PR #48](https://github.com/MateusAMP2119/iris-engine-cli/pull/48))
- [x] E06.2 Capture trigger emission — done ([PR #52](https://github.com/MateusAMP2119/iris-engine-cli/pull/52))
- [x] E06.3 Run attribution — done ([PR #55](https://github.com/MateusAMP2119/iris-engine-cli/pull/55))
- [x] E06.4 Payload tiers and modes — done ([PR #58](https://github.com/MateusAMP2119/iris-engine-cli/pull/58))
- [x] E06.5 Wipe replay and conflicts — done ([PR #60](https://github.com/MateusAMP2119/iris-engine-cli/pull/60))
- [x] E06.6 Promotion — done ([PR #89](https://github.com/MateusAMP2119/iris-engine-cli/pull/89))
- [x] E06.7 Live wipe closure — done ([PR #90](https://github.com/MateusAMP2119/iris-engine-cli/pull/90))

## E07 Provenance, Journal Lifecycle and Object Store — epic PR: pending

- [x] E07.1 Provenance walk — done ([PR #74](https://github.com/MateusAMP2119/iris-engine-cli/pull/74))
- [x] E07.2 Snapshot pin — done (merged via `issue/E07.2-snapshot-pin-rework`, commit 603f437)
- [x] E07.3 Seal and compaction — done ([PR #79](https://github.com/MateusAMP2119/iris-engine-cli/pull/79))
- [x] E07.4 Checkpoint chain and engine key — done ([PR #82](https://github.com/MateusAMP2119/iris-engine-cli/pull/82))
- [x] E07.5 Object store and export — done ([PR #86](https://github.com/MateusAMP2119/iris-engine-cli/pull/86))
- [x] E07.6 Archived reads and destroy closure — done ([PR #90](https://github.com/MateusAMP2119/iris-engine-cli/pull/90))

## E08 Build, Artifacts and Modes — epic PR: pending

- [x] E08.1 Recipe inference and matrix — done ([PR #62](https://github.com/MateusAMP2119/iris-engine-cli/pull/62))
- [x] E08.2 Build and artifact storage — done ([PR #66](https://github.com/MateusAMP2119/iris-engine-cli/pull/66))
- [x] E08.3 Promote gating — done ([PR #76](https://github.com/MateusAMP2119/iris-engine-cli/pull/76))
- [x] E08.4 Mode execution and retirement — done ([PR #91](https://github.com/MateusAMP2119/iris-engine-cli/pull/91))

## E09 Read API, Endpoints and PATs — epic PR: pending

- [x] E09.1 PAT store and scopes — done ([PR #53](https://github.com/MateusAMP2119/iris-engine-cli/pull/53))
- [x] E09.2 Endpoint compile and validation — done ([PR #56](https://github.com/MateusAMP2119/iris-engine-cli/pull/56))
- [x] E09.3 Param grammar and paging — done ([PR #57](https://github.com/MateusAMP2119/iris-engine-cli/pull/57))
- [x] E09.4 Envelope and serialization — done ([PR #61](https://github.com/MateusAMP2119/iris-engine-cli/pull/61))
- [x] E09.5 Route mux and auth — done ([PR #69](https://github.com/MateusAMP2119/iris-engine-cli/pull/69))
- [x] E09.6 Endpoint apply lifecycle — done ([PR #71](https://github.com/MateusAMP2119/iris-engine-cli/pull/71))
- [x] E09.7 Read pool and SQL safety — done ([PR #72](https://github.com/MateusAMP2119/iris-engine-cli/pull/72))
- [x] E09.8 Q and data routes — done ([PR #77](https://github.com/MateusAMP2119/iris-engine-cli/pull/77))
- [x] E09.9 NDJSON streaming — done ([PR #83](https://github.com/MateusAMP2119/iris-engine-cli/pull/83))
- [x] E09.10 Read parity closure — done ([PR #92](https://github.com/MateusAMP2119/iris-engine-cli/pull/92))

## E10 Destructive Operation Gates — epic PR: pending

- [x] E10.1 Gate and blocker predicates — done ([PR #75](https://github.com/MateusAMP2119/iris-engine-cli/pull/75))
- [x] E10.2 Confirmation flows — done ([PR #80](https://github.com/MateusAMP2119/iris-engine-cli/pull/80))
- [x] E10.3 Remote tiering and failover — done ([PR #87](https://github.com/MateusAMP2119/iris-engine-cli/pull/87))

## E11 High Availability and Failover — epic PR: pending

- [x] E11.1 Leader lock election — done ([PR #63](https://github.com/MateusAMP2119/iris-engine-cli/pull/63))
- [x] E11.2 Standby reads and rejection — done ([PR #100](https://github.com/MateusAMP2119/iris-engine-cli/pull/100))
- [x] E11.3 Promotion and self demotion — done ([PR #67](https://github.com/MateusAMP2119/iris-engine-cli/pull/67))
- [x] E11.4 Host prerequisites and live failover — done ([PR #102](https://github.com/MateusAMP2119/iris-engine-cli/pull/102))

## E12 Stats, Info and Inspect — epic PR: pending

- [x] E12.1 Stats rollups — done ([PR #64](https://github.com/MateusAMP2119/iris-engine-cli/pull/64))
- [x] E12.2 Info inspect and show — done ([PR #70](https://github.com/MateusAMP2119/iris-engine-cli/pull/70))

## E14 Graph Views and Triage Surface — epic PR: pending (builds before E13)

- [x] E14.1 Ref grammar and triage shows — done ([PR #81](https://github.com/MateusAMP2119/iris-engine-cli/pull/81))
- [x] E14.2 Workload wiring panel — done ([PR #84](https://github.com/MateusAMP2119/iris-engine-cli/pull/84))
- [x] E14.3 Rail renderer and golden files — done ([PR #109](https://github.com/MateusAMP2119/iris-engine-cli/pull/109))
- [x] E14.4 Read routes and before cursor — done ([PR #99](https://github.com/MateusAMP2119/iris-engine-cli/pull/99))

## E13 Golden Sample and Acceptance — epic PR: pending (the spine; last)

- [x] E13.1 Golden workspace fixture — done ([PR #94](https://github.com/MateusAMP2119/iris-engine-cli/pull/94))
- [x] E13.2 Install and binary boot — done ([PR #97](https://github.com/MateusAMP2119/iris-engine-cli/pull/97))
- [x] E13.3 Lane runs and failures — done ([PR #103](https://github.com/MateusAMP2119/iris-engine-cli/pull/103))
- [x] E13.4 Journal capture and wipe — done ([PR #98](https://github.com/MateusAMP2119/iris-engine-cli/pull/98))
- [x] E13.5 Sealing and archival — done ([PR #95](https://github.com/MateusAMP2119/iris-engine-cli/pull/95))
- [x] E13.6 Data provenance lineage — done ([PR #96](https://github.com/MateusAMP2119/iris-engine-cli/pull/96))
- [x] E13.7 Endpoint reads and grants — done ([PR #104](https://github.com/MateusAMP2119/iris-engine-cli/pull/104))
- [x] E13.8 Failover and unattended closure — done ([PR #111](https://github.com/MateusAMP2119/iris-engine-cli/pull/111))

## E15 Onboarding and Guided Tour — epic PR: pending (epic complete on development 2026-07-11)

- [x] E15.1 Quickstart surface and gating — done ([PR #133](https://github.com/MateusAMP2119/iris-engine-cli/pull/133)); also: startDetached argv rebuilt from cobra command (F2), spec §9 delta admitting spf13/pflag to the allowlist
- [x] E15.2 Tour orchestration and installer handoff — done ([PR #134](https://github.com/MateusAMP2119/iris-engine-cli/pull/134)); review fix: tour steps force-local, ambient IRIS_HOST/toml host ignored with notice (spec §8 clause)

## Cross-cutting devfix/debt PRs (2026-07-09/10 recovery session)

- [PR #93](https://github.com/MateusAMP2119/iris-engine-cli/pull/93) — api provenance redeclaration (post-#92 compile break; affects E07.6, E09.10)
- [PR #101](https://github.com/MateusAMP2119/iris-engine-cli/pull/101) — run_inputs FK-free per spec delta; capture grants in role provisioning (affects E05.9, E04.3)
- [PR #105](https://github.com/MateusAMP2119/iris-engine-cli/pull/105) — provenance CLI readout conformance rewrite (affects E13.6, S14 readout)
- [PR #106](https://github.com/MateusAMP2119/iris-engine-cli/pull/106) — deadletter plane, production stats, manual-run attribution, lint zero (affects E05.7, E05.8, E12.1, E04.4)
- [PR #107](https://github.com/MateusAMP2119/iris-engine-cli/pull/107) — read-pool/data-PAT provisioning idempotent on PG16+ non-superuser clusters (affects E09.7, E13.7)
- [PR #108](https://github.com/MateusAMP2119/iris-engine-cli/pull/108) — conformance lane stability: provisioning self-heals capture schema, suite isolation, CI timeout (affects E04.3, harness)
- [PR #109](https://github.com/MateusAMP2119/iris-engine-cli/pull/109) — orphaned S03/S04/S06.1/S08 contracts claimed; rail renderer (affects E14.3, E00.1)
- [PR #110](https://github.com/MateusAMP2119/iris-engine-cli/pull/110) — real seal: threshold gating, ed25519-signed checkpoint chain (affects E07.3, E07.4, E13.5)
- [PR #112](https://github.com/MateusAMP2119/iris-engine-cli/pull/112) — engine key in engine-owned meta table, spec delta; supersedes #110's workspace file (affects E07.4, E02.1)
- [PR #113](https://github.com/MateusAMP2119/iris-engine-cli/pull/113) — live engine-key reader: public half in `iris engine info`, private bytes never rendered (affects E12.2, E07.4)
- [PR #114](https://github.com/MateusAMP2119/iris-engine-cli/pull/114) — read-pool credential persisted in meta (converge-on-start); endpoint registry reloads from meta on restart; spec delta, roster 20 (affects E09.7, E09.6)
- [PR #115](https://github.com/MateusAMP2119/iris-engine-cli/pull/115) — leader advertisement: leadership meta table, standby guidance names the live leader, failover re-advertisement; spec delta, roster 21 (affects E11.2, E11.4)
- [PR #116](https://github.com/MateusAMP2119/iris-engine-cli/pull/116) — info/inspect/pipeline-show planes wired into the live daemon (affects E12.2)
- [PR #117](https://github.com/MateusAMP2119/iris-engine-cli/pull/117) — conformance flake hardening: freshDatabases isolation on leader-waiting tests, lane-wait headroom; PG16+ floor documented (harness)
- [PR #118](https://github.com/MateusAMP2119/iris-engine-cli/pull/118) — production sources for /runs, /runs/{id}/trace, /pipelines/{name}/gate; `iris run list` serves live (affects E14.3, E14.4)
- [PR #120](https://github.com/MateusAMP2119/iris-engine-cli/pull/120) — README (banner, quick install), install.sh, tag-driven release workflow; master checkpoint, tagged v0.1.0 (first public release: 4 platform tarballs + checksums)
- [PR #121](https://github.com/MateusAMP2119/iris-engine-cli/pull/121) — `iris --version` surface: internal/buildinfo leaf, ldflags stamp in release.yml, contracts S08/version-flag-defaults-dev + S08/version-template-format; spec §8 Q/A + §10 roster delta
- [PR #122](https://github.com/MateusAMP2119/iris-engine-cli/pull/122) — v0.1.1 checkpoint: version surface + POSIX-sh installer (bash-free); quick install verified end to end (`curl | sh` → `iris version v0.1.1`)
- [PR #123](https://github.com/MateusAMP2119/iris-engine-cli/pull/123) — installer polish: uninstall.sh (managed-path-only, daemon-teardown guard) + rainbow install banner
- [PR #124](https://github.com/MateusAMP2119/iris-engine-cli/pull/124) — `iris engine update` self-update: internal/update stdlib leaf, redirect tag resolution, sha256 verify, atomic replace; contracts S08/update-{dev-build-refuses,tag-equals-up-to-date,verified-atomic-replace,checksum-mismatch-aborts}; spec §2/§8/§10 deltas
- [PR #126](https://github.com/MateusAMP2119/iris-engine-cli/pull/126) — interactive uninstall.sh: /dev/tty confirmation boxes, daemon-warning override prompt, rainbow goodbye
- [PR #127](https://github.com/MateusAMP2119/iris-engine-cli/pull/127) — root-level `iris uninstall` self-removal: consent gate (--yes/--force, tty prompt), daemon-reachable refusal, EvalSymlinks self-remove; contracts S08/uninstall-{root-verb,consent-gate,daemon-running-refused,removes-executable}; amends S08/resource-first-command-tree + S02/daemonless-lifecycle-commands (authorized spec delta)
- [PR #129](https://github.com/MateusAMP2119/iris-engine-cli/pull/129) — self-update verb relocated to root `iris update` (spec delta amending S08/resource-first-command-tree + S02/daemonless-lifecycle-commands; top level = nine nouns + update + uninstall)
- [PR #131](https://github.com/MateusAMP2119/iris-engine-cli/pull/131) — terminal ceremony for iris update/uninstall: paint.go tty/NO_COLOR/json-gated ANSI, update journey via Updater.Progress hook, aligned uninstall box + rainbow farewell, slog double-report removed; contracts S08/lifecycle-ceremony-{tty-gating,plain-when-piped} + S08/update-progress-stages
