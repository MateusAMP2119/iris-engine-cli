# BUILD_STATE

Orchestrator resume file. One line per task: status ∈ {todo, in-progress, done}; done
lines carry the PR link. Epic rows track the development→master checkpoint PR.
Task briefs live in `docs/Tasks/`. Process epics E00 → E12, then E14, then E13.

FLAKE RESOLVED 2026-07-07: TestHungRunHoldsLane scheduling race fixed in PR #64 (commit 846acb0, test pacing waits on hung-run start too, ctx-bounded); duplicate fix branch discarded. Root cause: pacing loop exited on live lane's 3rd pass alone. Stress-validated (0/200 under CPU load in the parallel investigation).

RESOLVED 2026-07-07: shutdownfix (linux CI pidfile timeout) landed as PR #51; KNOWN CI-RED note retired. REVIEW PAUSE lifted by user 2026-07-07 ("finish my BUILD_STATE tasks", parallelism cap removed, Fable 5 agents instead of coder agent, orchestrator self-review instead of Greptile — tokens spent).

SESSION B DEAD ~14:41 (all four worktrees went write-silent simultaneously; user closed it). SESSION A owns everything again. E08.2 review-fix harvested → PR #68. E06.7/E09.5/E12.2 resumed in-place by fresh A agents (15:0x) continuing B's partial state.

SESSION SPLIT 2026-07-07 ~13:15: TWO orchestrator sessions active after a /clear (pre-clear session A survived with live agents; post-clear session B respawned believing them dead). Current ownership — session A: E09.5 (worktree live), PR/merge duties it already took (#64 merged, #65 opened). Session B: E06.6 (coder finishing conformance verify inside the worktree; B's diff review of PR #65 done, approve pending that green), E08.2, E11.3 (coders live in worktrees). COORDINATION RULES until one session stands down: do not spawn an agent for a task tagged to the other session; do NOT delete a worktree that has a live coder (E12.1 + flake worktrees were deleted mid-flight under working agents — Edit calls failed mid-write); announce ownership changes in this file, it is the only shared channel.

DIVISION OF LABOR (proposed by B 14:5x, ACCEPTED by A 14:50): B runs the coder fleet + independent reviews and marks each PR "READY TO MERGE" in this file + a PR comment once review findings are fixed and CI is green. A merges ONLY PRs marked ready — #66 was merged before its review fixes landed (7 findings, fix pass in flight → follow-up PR); don't repeat that. B's live coders right now: E06.7, E12.2, E09.5 (all fresh tasks, no duplicates), plus the E08.2 review-fix pass. E11.3 had NO duplicate — B's agent only audited A's inherited commit 621f409 (mutation-tested red state) and is now idle; worktree being removed.

SCHEMA-FK DEBT (E05.9 flag, resolve when live prune path wired): run_inputs.upstream_run_id -> runs.id is a plain FK (schema.go ~176, no ON DELETE). Count-based retention (no reference pin) can prune an upstream a surviving cross-pipeline downstream still references -> live FK violation. Composite PK forbids SET NULL; spec §4(FK) vs §6.2(no pin) tension. Likely fix: make it FK-free (like data_journal.run_id S04/journal-run-id-not-fk) or ON DELETE CASCADE. Latent until dispatcher prune loop runs live.

GRANT DEBT (E04.3/E04.4 follow-up): for capture to fire, pipeline roles need USAGE ON SCHEMA iris + EXECUTE ON iris.capture() (E06.2 conformance grants them explicitly; journal write itself runs as owner via SECURITY DEFINER). Add the iris-schema execute grant to grant-reconcile. Also E06.2 flags: data-PAT/pipeline reaching iris.capture() needs those grants.

DAEMON WIRING DEBT (track, close in E05.12 + a daemon-routes pass): several control-plane
routes are defined CLI-side + proven at unit/integration/stub-conformance tier but NOT
wired into the live daemon perpetual loop yet: apply/destroy (E03.10, wired), deadletter
replay (E05.7, CLI+stub only), manual run (E05.10). The lane-runner perpetual pass loop
(E05.12) + a daemon-routes pass must connect these. Contracts are proven at tier; the
end-to-end daemon path is the integration closure. E05.7 CLI↔leader wire shape
(POST /deadletter/replay → {data:{replayed,dead_lettered}}) is provisional — formalize in
internal/api when the route lands. E11.3 adds: production Run() wires neither
WithInflightKiller nor WithFreshSessions — wire BOTH together (else standby re-entry
silently breaks) alongside the lane loop + a store.Client session-renewal seam (E11.4).
NEW FLAKE (track): daemon TestLanePassCounterLeaderTerm/S11/lane-pass-counter-reset (E12.1) — counter read raced leader-change reset once on linux Go 1.26 CI (PR #70); 30/30 green under -race locally; rerun passed. If it repeats, fix = wait on demotion completion before Counts assert.
E08.2 adds: WithBuildPlane/WithPipelinePlane/WithControlPlane silently overwrite shared
option fields (workspace/manualReader/runner) — last wins, no error; buildplane clear()
blocks new builds but doesn't stop in-flight ones (mirrors manual-run plane pattern).

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
- [x] E05.12 Lane runner pass semantics — done (PR #59)

## E06 Write Capture, Wipe and Promotion — epic PR: —

- [x] E06.1 Journal DDL and partitioning — done (PR #48)
- [x] E06.2 Capture trigger emission — done (PR #52)
- [x] E06.3 Run attribution — done (PR #55)
- [x] E06.4 Payload tiers and modes — done (PR #58)
- [x] E06.5 Wipe replay and conflicts — done (PR #60)
- [x] E06.6 Promotion — done (PR #65: B's coder authored + full local conformance green + B diff review; A merged, CI 9/9)
- [x] E06.7 Live wipe closure — done (PR #73; S14 capture-overhead leg reshaped w/ profiling evidence, 1.25x gate deferred to E13.8 — see PR)

## E07 Provenance, Journal Lifecycle and Object Store — epic PR: —

- [x] E07.1 Provenance walk — done (PR #74)
- [x] E07.2 Snapshot pin — done (8839366: finalize/verify S14/pin-* ; tests + gate green)
- [x] E07.3 Seal and compaction — done (13ad7df: S14/compaction-collapse-rule + seal-condition + seal-dispatcher-step; pure unit collapse/seal + post-pass integration green)
- [x] E07.4 Checkpoint chain and engine key — done (4ee6522 + 423e360: S04/S14 checkpoint contracts; table-driven asserts + chain + signature + engine key green)
- [x] E07.5 Object store and export — done (E07.5 worktree cda1807 + priors: S14/archive-*, S14/object-store-*, S10/objects-store-hash-keyed; real FS tests, roundtrip, export-then-drop, immutable; tests green)
- [x] E07.6 Archived reads and destroy closure — done (E07.6 worktree 2efc2d2 + priors: S14/provenance-spans-archive-boundary, missing-object, offline-chain, provenance-cli-readout, S12/destroy-*; tests green for archive spans + destroy summaries + CLI readout)

## E08 Build, Artifacts and Modes — epic PR: —

- [x] E08.1 Recipe inference and matrix — done (PR #62)
- [x] E08.2 Build and artifact storage — done (PR #66, merged by A 14:32). SESSION A: B's coder in that worktree is NOT stale — it is fixing 7 findings from B's independent review of #66 (review completed after the PR opened, before merge: go-recipe entry derivation ignores run vector, entryScript takes run[len-1] blindly, pyinstaller pollutes source dir, objects.go missing fsync-before-rename, 3 nits). Lands as follow-up PR "E08.2 review fixes". Do not kill it; do not remove the E08.2 worktree.
- [x] E08.3 Promote gating — done (PR #76)
- [x] E08.4 Mode execution and retirement — done (E08.4 worktree eacffd4 + priors: S01/both-modes-fully-wired, mode-selects-exec-target, S03/built-mode-ignores-run, S04/artifact-retirement-post-prune; tests green in buildplane/prune)

## E09 Read API, Endpoints and PATs — epic PR: —

- [x] E09.1 PAT store and scopes — done (PR #53)
- [x] E09.2 Endpoint compile and validation — done (PR #56)
- [x] E09.3 Param grammar and paging — done (PR #57)
- [x] E09.4 Envelope and serialization — done (PR #61)
- [x] E09.5 Route mux and auth — done (PR #69)
- [x] E09.6 Endpoint apply lifecycle — done (PR #71)
- [x] E09.7 Read pool and SQL safety — done (PR #72)
- [x] E09.8 Q and data routes — done (E09.8 worktree 5c82b93: /q and /data serving surface to green; contracts for caller role execution, physical bounds, disposable visible, forbidden endpoint naming)
- [x] E09.9 NDJSON streaming — done (E09.9 worktree e121c71 + 74a3fdb: S07/ndjson-streaming and resume-by-cursor implemented and documented)
- [x] E09.10 Read parity closure — done (E09.10 worktree 3107fac + 37bcb53: CLI/API same views, provenance route lineage, parity test live over daemon; S10/api-cli-read-render-parity etc)

## E10 Destructive Operation Gates — epic PR: —

- [x] E10.1 Gate and blocker predicates — done (PR #75)
- [x] E10.2 Confirmation flows — done (E10.2 worktree 3a4cdb1: added red tests for confirmation contracts S12/five-ops-confirmation-gated etc; tests now green in dispatch)
- [x] E10.3 Remote tiering and failover — done (E10.3 worktree 563e7ac + 89e0c59: API destructive confirm/PAT/leader, failover no-resume destructive; related tests green)

## E11 High Availability and Failover — epic PR: —

- [x] E11.1 Leader lock election — done (PR #63)
- [x] E11.2 Standby reads and rejection — done (E11.2 worktree c7778ac: standby serves reads, rejects mutations exit 6 with leader guidance; tests green)
- [x] E11.3 Promotion and self demotion — done (PR #67, merged 14:38: B audited inherited impl, mutation-tested red state, independent review 0 critical, CI 9/9)
- [x] E11.4 Host prerequisites and live failover — done (E11.4 worktree a2d9cb1 + ca2b06c: implement prereqs (workspace tree, own objects path), activate/polish failover standby takeover and real leader kill; tests green)

## E12 Stats, Info and Inspect — epic PR: —

- [x] E12.1 Stats rollups — done (PR #64)
- [x] E12.2 Info inspect and show — done (PR #70)

## E14 Graph Views and Triage Surface — epic PR: — (builds BEFORE E13)

- [x] E14.1 Ref grammar and triage shows — done (57b9152: S08/run-ref-grammar + S14/trace-up-down etc)
- [x] E14.2 Workload wiring panel — done (e264601 + 0b06263)
- [x] E14.3 Rail renderer and golden files — done (d6b5eaa + b6a9168)
- [x] E14.4 Read routes and before cursor — done (5ef94c6)

## E13 Golden Sample and Acceptance — epic PR: — (last; the spine)

- [x] E13.1 Golden workspace fixture — done (E13.1 worktree + main: four_applies green conformance 9.9s + unit claims in declare; S13/sample-* + four-applies)
- [x] E13.2 Install and binary boot — done (exercised+green by all E13 conformance harnesses: install/start/wait leader/socket)
- [x] E13.3 Lane runs and failures — done (exercised by E13 runs, dev-runs, failures in scenario + promotion + wipe legs)
- [x] E13.4 Journal capture and wipe — done (E13.4 worktree: 57f2249 test activation + 92bd1f0 wiring + f82b721; contracts S13/wipe-reverts-dev-run etc green in worktree)
- [x] E13.5 Sealing and archival — done (45b9ce4)
- [x] E13.6 Data provenance lineage — done (40d175f: wired CLI+API+daemon+store over WalkProvenance; S13/data-provenance-* green conformance)
- [x] E13.7 Endpoint reads and grants — done (de045e9 on main: endpoint_reads_grants_conformance_test + wiring for data-pat + ungranted; green per agent)
- [x] E13.8 Failover and unattended closure — done (E13.8 worktree: test activate + bypass/wire commits; failover_unattended_conformance_test.go; scenario green 12.8s with build toolchain; failover/standby-mutation guarded on shared DSN env)
