export const meta = {
  name: 'pr-review',
  description: 'Multi-dimension review of an Iris PR diff, adversarial verify of findings',
  whenToUse: 'Before merging any issue PR into development or epic PR into master. args: {base, head, scope} — base/head are git refs; scope is a one-line description of the change under review.',
  phases: [
    { title: 'Review', detail: 'six dimension reviewers over the PR diff' },
    { title: 'Verify', detail: 'adversarial refutation of each finding' },
  ],
}

const REPO = '/Users/mateuscosta/Development/iris-engine+cli'
const base = (args && args.base) || 'origin/development'
const head = (args && args.head) || 'HEAD'
const scope = (args && args.scope) || 'the PR diff'

const FINDINGS_SCHEMA = {
  type: 'object',
  properties: {
    findings: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          file: { type: 'string' },
          line: { type: 'number' },
          severity: { type: 'string', enum: ['critical', 'major', 'minor', 'nit'] },
          category: { type: 'string' },
          summary: { type: 'string' },
          detail: { type: 'string' },
          failure_scenario: { type: 'string' },
        },
        required: ['file', 'severity', 'summary', 'detail', 'failure_scenario'],
      },
    },
  },
  required: ['findings'],
}

const VERDICT_SCHEMA = {
  type: 'object',
  properties: {
    refuted: { type: 'boolean' },
    confidence: { type: 'string', enum: ['high', 'medium', 'low'] },
    reasoning: { type: 'string' },
  },
  required: ['refuted', 'confidence', 'reasoning'],
}

const COMMON = `You are reviewing a PR in the Iris engine repo at ${REPO}. Scope: ${scope}. The diff under review is \`git -C ${REPO} diff ${base}...${head}\` — ALWAYS pass -C ${REPO}; your working directory may be elsewhere and must not influence what you review. Review exactly THAT diff (never your CWD's HEAD or working tree), using the rest of the tree at ${REPO} as context. Read changed files via \`git -C ${REPO} show ${head}:<path>\` or the repo path. NEVER read, grep, or report on anything under ${REPO}/.worktrees/ — those are other branches' checkouts; findings there are out of scope and will be discarded. Report only defects introduced by THIS diff's changed files. Read CLAUDE.md first (conventions + TDD doctrine). Spec: docs/Iris Specification Inventory.md (source of truth); epic contract tables: docs/Iris Epics.md; task briefs: docs/Tasks/.
Report ONLY defects anchored to a file (and line where possible) with a concrete failure scenario: inputs/state -> wrong outcome. No praise, no restating accepted tree style, no scope creep into later epics (absence of future-epic features is NOT a finding). Severity: critical = wrong behavior vs spec/contract or data-loss class; major = real bug or spec deviation likely to bite later; minor = defensible fix; nit = polish. Drop anything you cannot back with actual code text.`

phase('Review')
const DIMENSIONS = [
  {
    key: 'logic-correctness',
    prompt: `${COMMON}
Dimension: logic correctness of the changed non-test code. Hunt: edge cases the new code mishandles, off-by-one/boundary errors, error paths swallowed or mis-wrapped, invariants violated, incorrect algorithm vs the contract's Behavior text (docs/Iris Epics.md), nil/zero-value traps, YAML/parse edge cases. Trace code paths concretely; you may run go test or read-only probes.`,
  },
  {
    key: 'runtime-concurrency',
    prompt: `${COMMON}
Dimension: runtime behavior of the changed code: races, goroutine leaks, process/fd/socket lifecycle, cancellation and context threading, deadlocks, cleanup ordering, resource leaks, platform assumptions (unix-only code paths). You may run go test -race read-only probes.`,
  },
  {
    key: 'tdd-honesty',
    prompt: `${COMMON}
Dimension: TDD honesty of the changed tests. For every contract id claimed in the diff (// spec: or subtest path), compare the test against the contract's Behavior text in docs/Iris Epics.md: does the test genuinely prove it? Hunt vacuous assertions, tests passing on empty/wrong output, fakes tested where the contract demands the real seam, fixed sleeps, weakened expectations vs spec text. Report only claims that do NOT hold.`,
  },
  {
    key: 'spec-manifest',
    prompt: `${COMMON}
Dimension: spec/manifest fidelity of the change. If spec/contracts.yaml or spec/inventory.lock changed: verify row edits against the epic tables (id, tier, status semantics). For the task's contracts: verify the implementation matches the spec sections the contract anchors name (read them). Hunt: behavior the spec text requires but the diff's implementation lacks within its own scope, or contradicts (naming, enums, shapes, exit codes, file formats).`,
  },
  {
    key: 'ci-lint-config',
    prompt: `${COMMON}
Dimension: CI/config hygiene. If .github/workflows/, .golangci.yml, go.mod, or build tags changed: audit correctness (matrix quoting, job gating, pinned versions, cache keys, service containers, DSNs, new lint excludes justified narrowly). If unchanged by this diff, check instead that the diff does not BREAK existing CI assumptions (new packages picked up by test/build/lint jobs, build tags covered, new deps allowed by CLAUDE.md allowlist). Report an empty findings list if genuinely nothing.`,
  },
  {
    key: 'conventions-arch',
    prompt: `${COMMON}
Dimension: conventions + architecture vs CLAUDE.md and spec section 10. Hunt in the diff: import-direction violations (cli -> daemon/api -> dispatch -> store/pg/exec; archive beside dispatch; declare/build/pat leaves), mutable package globals, fmt.Print/log instead of slog in non-test code, cross-package panics, missing doc comments on new exported identifiers, %w gaps, context gaps on blocking calls, seam shapes contradicting spec section 4/10 table shapes or naming (enum spellings, column names), dependency allowlist violations.`,
  },
]

const results = await pipeline(
  DIMENSIONS,
  (d) => agent(d.prompt, { label: `review:${d.key}`, phase: 'Review', schema: FINDINGS_SCHEMA, model: 'opus' }),
  (review, d) => {
    if (!review || !review.findings || review.findings.length === 0) return []
    const capped = review.findings.slice(0, 12)
    if (review.findings.length > 12) log(`review:${d.key} returned ${review.findings.length} findings; verifying top 12`)
    return parallel(
      capped.map((f) => () => {
        const nVoters = f.severity === 'critical' || f.severity === 'major' ? 3 : 1
        const lenses = ['correctness', 'spec-text', 'reproduce'].slice(0, nVoters)
        return parallel(
          lenses.map((lens) => () =>
            agent(
              `${COMMON}
You are an adversarial verifier (lens: ${lens}). A reviewer claims this defect. Try hard to REFUTE it by reading the actual code and spec text. Default to refuted=true if the failure cannot actually occur, is out of this PR's scope (later epic), contradicts accepted tree conventions, or misreads the spec.
Lens ${lens}: ${lens === 'correctness' ? 'trace the code path concretely — can the failure state arise?' : lens === 'spec-text' ? 'check the exact spec/contract wording — does it require what the reviewer assumes?' : 'attempt a read-only reproduction (go test / scratch probe in /tmp) if feasible; else reason step by step.'}
Finding: [${f.severity}] ${f.file}${f.line ? ':' + f.line : ''} — ${f.summary}
Detail: ${f.detail}
Claimed failure scenario: ${f.failure_scenario}`,
              { label: `verify:${f.file.split('/').pop()}`, phase: 'Verify', schema: VERDICT_SCHEMA, model: 'opus' },
            ),
          ),
        ).then((votes) => {
          const valid = votes.filter(Boolean)
          const surviving = valid.length > 0 && valid.filter((v) => !v.refuted).length > valid.length / 2
          return { ...f, dimension: d.key, surviving, votes: valid.map((v) => ({ refuted: v.refuted, confidence: v.confidence, reasoning: v.reasoning.slice(0, 300) })) }
        })
      }),
    )
  },
)

const all = results.filter(Boolean).flat().filter(Boolean)
const confirmed = all.filter((f) => f.surviving)
const refuted = all.filter((f) => !f.surviving)
log(`${all.length} raw findings, ${confirmed.length} confirmed, ${refuted.length} refuted`)
return {
  confirmed: confirmed.map((f) => ({ severity: f.severity, dimension: f.dimension, file: f.file, line: f.line, summary: f.summary, detail: f.detail, failure_scenario: f.failure_scenario, votes: f.votes })),
  refuted: refuted.map((f) => ({ severity: f.severity, file: f.file, summary: f.summary })),
}