# BUILD_STATE

Orchestrator resume file. One line per task: status ∈ {todo, in-progress, done}; done
lines carry the PR link. Epic rows track the development→master checkpoint PR.
Task briefs live in `docs/Tasks/`. Process epics E00 → E12, then E14, then E13.

KNOWN CI-RED (fixing, worktree .worktrees/shutdownfix): conformance TestSignalGracefulShutdown/SIGINT fails on LINUX CI only (passes darwin) — pidfile removal gated behind election-lock release + embedded-PG teardown (both heavier since E05.10 added the data pool + pipeline plane), crosses the test 10s deadline. Fix: remove pidfile promptly after Serve returns. E05.8 (PR #50) merge HOLDS until this lands + re-sync.

SCHEMA-FK DEBT (E05.9 flag, resolve when live prune path wired): run_inputs.upstream_run_id -> runs.id is a plain FK (schema.go ~176, no ON DELETE). Count-based retention (no reference pin) can prune an upstream a surviving cross-pipeline downstream still references -> live FK violation. Composite PK forbids SET NULL; spec §4(FK) vs §6.2(no pin) tension. Likely fix: make it FK-free (like data_journal.run_id S04/journal-run-id-not-fk) or ON DELETE CASCADE. Latent until dispatcher prune loop runs live.

GRANT DEBT (E04.3/E04.4 follow-up): for capture to fire, pipeline roles need USAGE ON SCHEMA iris + EXECUTE ON iris.capture() (E06.2 conformance grants them explicitly; journal write itself runs as owner via SECURITY DEFINER). Add the iris-schema execute grant to grant-reconcile. Also E06.2 flags: data-PAT/pipeline reaching iris.capture() needs those grants.

DAEMON WIRING DEBT (track, close in E05.12 + a daemon-routes pass): several control-plane
routes are defined CLI-side + proven at unit/integration/stub-conformance tier but NOT
wired into the live daemon perpetual loop yet: apply/destroy (E03.10, wired), deadletter
replay (E05.7, CLI+stub only), manual run (E05.10). The lane-runner perpetual pass loop
(E05.12) + a daemon-routes pass must connect these. Contracts are proven at tier; the
end-to-end daemon path is the integration closure. E05.7 CLI↔leader wire shape
(POST /deadletter/replay → {data:{replayed,dead_lettered}}) is provisional — formalize in
internal/api when the route lands.

Worktrees: `../iris-worktrees/EXX.Y` on branch `issue/EXX.Y-short-name`.

RECONCILED 2026-07-07 07:xx: PRs #27 (E03.2 lanes) + #33 (test flake fix) had silently NOT merged despite chains reporting success (API-instability window) — merged into development by hand (commits 6afe150, e131edd); development green, daemon race-clean x3. AUDIT LESSON: verify each merge landed (grep a signature file on origin/development) before marking done; don't trust merge-when-green.sh exit alone.

## E00 Conformance Harness and Traceability Gate — epic PR: #6 (merged)

Post-epic deep review (multi-agent, orchestrator-run per user instruction) produced
review-fix PRs #7 (CI/lint/conformance), #8 (gate hardening, 3 rounds), #9 (exec
runner rebuilt on stdlib os/exec + WaitDelay, 3 rounds) — all reviewed to zero
confirmed findings and merged. Standing rule: every PR gets the Opus pr-review
workflow (.claude/workflows/pr-review.js) before merge; on stall, kill + restart on
Opus, never downgrade.

- [x] E00.1 Manifest seed and doctrine — done (PR #1: https://github.com/MateusAMP2119/iris-engine-cli/pull/1)
- [x] E00.2 Traceability gate — done (PR #2: https://github.com/MateusAMP2119/iris-engine-cli/pull/2)
- [x] E00.3 Golden files and fixtures — done (PR #3: https://github.com/MateusAMP2119/iris-engine-cli/pull/3)
- [x] E00.4 Fakes and process IO — done (PR #5: https://github.com/MateusAMP2119/iris-engine-cli/pull/5)
- [x] E00.5 Conformance runner and CI — done (PR #4: https://github.com/MateusAMP2119/iris-engine-cli/pull/4)

## E01 Repo Skeleton, CLI Frame and Config — epic PR: #14 (merged)

- [x] E01.1 Module and package layout — done (PR #10: https://github.com/MateusAMP2119/iris-engine-cli/pull/10)
- [x] E01.2 Cobra tree and exit codes — done (PR #12: https://github.com/MateusAMP2119/iris-engine-cli/pull/12)
- [x] E01.3 Config precedence — done (PR #13: https://github.com/MateusAMP2119/iris-engine-cli/pull/13)
- [x] E01.4 CI and lint wiring — done (PR #11: https://github.com/MateusAMP2119/iris-engine-cli/pull/11)

## E02 Engine Install, Daemon and Leadership — epic PR: #23 (merged)

- [x] E02.1 Meta DDL and schema — done (PR #15: https://github.com/MateusAMP2119/iris-engine-cli/pull/15)
- [x] E02.2 Admin DSN chain — done (PR #16: https://github.com/MateusAMP2119/iris-engine-cli/pull/16)
- [x] E02.3 Managed Postgres subprocess — done (PR #17: https://github.com/MateusAMP2119/iris-engine-cli/pull/17)
- [x] E02.4 Install and uninstall — done (PR #18: https://github.com/MateusAMP2119/iris-engine-cli/pull/18)
- [x] E02.5 Listeners and daemon protocol — done (PR #19: https://github.com/MateusAMP2119/iris-engine-cli/pull/19)
- [x] E02.6 Leader election single writer — done (PR #20: https://github.com/MateusAMP2119/iris-engine-cli/pull/20)
- [x] E02.7 Crash reconciliation — done (PR #22: https://github.com/MateusAMP2119/iris-engine-cli/pull/22)
- [x] E02.8 Logging and service unit — done (PR #21: https://github.com/MateusAMP2119/iris-engine-cli/pull/21)

## E03 Declarations, Schemas and Apply — epic PR: #36 (Greptile 7 findings; epic-fix in-progress worktree .worktrees/epicE03fix; merge to master after)

- [x] E03.1 Declaration parsing and discovery — done (PR #24: https://github.com/MateusAMP2119/iris-engine-cli/pull/24)
- [x] E03.2 Lane composer validation — done (PR #27: https://github.com/MateusAMP2119/iris-engine-cli/pull/27)
- [x] E03.3 Single file targets — done (PR #26: https://github.com/MateusAMP2119/iris-engine-cli/pull/26; Sonnet)
- [x] E03.4 Dependency graph validation — done (PR #25: https://github.com/MateusAMP2119/iris-engine-cli/pull/25)
- [x] E03.5 Type mapping and DDL — done (PR #28: https://github.com/MateusAMP2119/iris-engine-cli/pull/28)
- [x] E03.6 Drift classification — done (PR #30: https://github.com/MateusAMP2119/iris-engine-cli/pull/30)
- [x] E03.7 Migration ledger sync — done (PR #31: https://github.com/MateusAMP2119/iris-engine-cli/pull/31)
- [x] E03.8 Idempotent provisioning — done (PR #32: https://github.com/MateusAMP2119/iris-engine-cli/pull/32)
- [x] E03.9 Registry persistence in meta — done (PR #29: https://github.com/MateusAMP2119/iris-engine-cli/pull/29)
- [x] E03.10 Apply destroy closure — done (PR #35: https://github.com/MateusAMP2119/iris-engine-cli/pull/35)

## E04 Roles, Grants and Credentials — epic PR: — (complete on development; batched into next dev→master checkpoint with E05)

- [x] E04.1 Access declaration validation — done (PR #34: https://github.com/MateusAMP2119/iris-engine-cli/pull/34; Sonnet)
- [x] E04.2 Role and credential lifecycle — done (PR #37: https://github.com/MateusAMP2119/iris-engine-cli/pull/37)
- [x] E04.3 Grant reconcile and drift — done (PR #43)
- [x] E04.4 Connection injection and enforcement — done (PR #46)

## E05 Dispatcher, Lanes and Dead Letters — epic PR: —

- [x] E05.1 Exec seam — done (PR #39: https://github.com/MateusAMP2119/iris-engine-cli/pull/39)
- [x] E05.2 Run environment — done (PR #40)
- [x] E05.3 Run records and states — done (PR #42)
- [x] E05.4 Lane model and walk — done (PR #38: https://github.com/MateusAMP2119/iris-engine-cli/pull/38)
- [x] E05.5 Gate and consumption — done (PR #44)
- [x] E05.6 Failure propagation — done (PR #45)
- [x] E05.7 Dead letter replay — done (PR #47)
- [x] E05.8 Dead letter drain — done (PR #50)
- [x] E05.9 Retention and pruning — done (PR #54)
- [x] E05.10 Manual pipeline run — done (PR #49)
- [x] E05.11 Doctrines and scope — done (verification-only: all 5 exempt rows seeded by E00.1, gate-accounted; no PR needed)
- [ ] E05.12 Lane runner pass semantics — todo (needs E05.1, E05.4, E05.5)

## E06 Write Capture, Wipe and Promotion — epic PR: —

- [x] E06.1 Journal DDL and partitioning — done (PR #48)
- [x] E06.2 Capture trigger emission — done (PR #52)
- [x] E06.3 Run attribution — done (PR #55)
- [ ] E06.4 Payload tiers and modes — todo (needs E06.2, E06.3)
- [ ] E06.5 Wipe replay and conflicts — todo (needs E06.1)
- [ ] E06.6 Promotion — todo (needs E06.5)
- [ ] E06.7 Live wipe closure — todo (needs E06.5, E06.6)

## E07 Provenance, Journal Lifecycle and Object Store — epic PR: —

- [ ] E07.1 Provenance walk — todo (needs E05, E06)
- [ ] E07.2 Snapshot pin — todo (needs E05, E06)
- [ ] E07.3 Seal and compaction — todo (needs E05, E06)
- [ ] E07.4 Checkpoint chain and engine key — todo (needs E07.3)
- [ ] E07.5 Object store and export — todo (needs E07.4)
- [ ] E07.6 Archived reads and destroy closure — todo (needs E07.1, E07.5)

## E08 Build, Artifacts and Modes — epic PR: —

- [ ] E08.1 Recipe inference and matrix — todo (needs E03, E05)
- [ ] E08.2 Build and artifact storage — todo (needs E08.1)
- [ ] E08.3 Promote gating — todo (needs E08.2)
- [ ] E08.4 Mode execution and retirement — todo (needs E08.2)

## E09 Read API, Endpoints and PATs — epic PR: —

- [x] E09.1 PAT store and scopes — done (PR #53)
- [ ] E09.2 Endpoint compile and validation — todo (needs E02, E03, E04)
- [ ] E09.3 Param grammar and paging — todo (needs E09.2)
- [ ] E09.4 Envelope and serialization — todo (needs E09.3)
- [ ] E09.5 Route mux and auth — todo (needs E09.1, E09.4)
- [ ] E09.6 Endpoint apply lifecycle — todo (needs E09.2, E09.5)
- [ ] E09.7 Read pool and SQL safety — todo (needs E09.1, E09.5)
- [ ] E09.8 Q and data routes — todo (needs E09.6, E09.7)
- [ ] E09.9 NDJSON streaming — todo (needs E09.5, E09.8)
- [ ] E09.10 Read parity closure — todo (needs E09.8)

## E10 Destructive Operation Gates — epic PR: —

- [ ] E10.1 Gate and blocker predicates — todo (needs E03, E05, E06)
- [ ] E10.2 Confirmation flows — todo (needs E10.1)
- [ ] E10.3 Remote tiering and failover — todo (needs E10.2)

## E11 High Availability and Failover — epic PR: —

- [ ] E11.1 Leader lock election — todo (needs E02, E05)
- [ ] E11.2 Standby reads and rejection — todo (needs E11.1)
- [ ] E11.3 Promotion and self demotion — todo (needs E11.1)
- [ ] E11.4 Host prerequisites and live failover — todo (needs E11.3; conformance rows ride E13 step 9)

## E12 Stats, Info and Inspect — epic PR: —

- [ ] E12.1 Stats rollups — todo (needs E02, E05)
- [ ] E12.2 Info inspect and show — todo (needs E12.1)

## E14 Graph Views and Triage Surface — epic PR: — (builds BEFORE E13)

- [ ] E14.1 Ref grammar and triage shows — todo (needs E05, E07, E09)
- [ ] E14.2 Workload wiring panel — todo (needs E14.1)
- [ ] E14.3 Rail renderer and golden files — todo (needs E05, E07, E09)
- [ ] E14.4 Read routes and before cursor — todo (needs E14.1, E14.2)

## E13 Golden Sample and Acceptance — epic PR: — (last; the spine)

- [ ] E13.1 Golden workspace fixture — todo (needs E00; grows with all epics)
- [ ] E13.2 Install and binary boot — todo (needs E13.1)
- [ ] E13.3 Lane runs and failures — todo (needs E13.1, E13.2)
- [ ] E13.4 Journal capture and wipe — todo (needs E13.3)
- [ ] E13.5 Sealing and archival — todo (needs E13.4)
- [ ] E13.6 Data provenance lineage — todo (needs E13.5)
- [ ] E13.7 Endpoint reads and grants — todo (needs E13.1, E13.6)
- [ ] E13.8 Failover and unattended closure — todo (needs all earlier E13)
