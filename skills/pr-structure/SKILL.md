---
name: pr-structure
description: "Use when drafting, opening, or updating a pull request in iris-lakehouse. Enforces compact PR bodies: short summary, change bullets, and required runtime evidence (CI alone is not enough)."
version: 1.0.0
author: Iris
license: MIT
metadata:
  hermes:
    tags: [pr, github, review, iris]
    related_skills: []
---

# PR structure (iris-lakehouse)

## Overview

PRs in this repo stay **clean and compact**. Reviewers should see what changed and proof it behaves — not a novel, not a file dump, and not “CI is green” as the only verification.

GitHub pre-fills bodies from `.github/PULL_REQUEST_TEMPLATE.md`. This skill is the agent-facing rules for filling that template (or rewriting a weak body before open/update).

## When to Use

- Opening a PR (`gh pr create`, web UI, or equivalent)
- Updating a PR description after new commits
- Reviewing your own draft body before publish

Don't use for: commit message style alone, issue writeups, or epic planning docs.

## Required body shape

Use this order. Keep each section tight.

### 1. Summary

2–4 sentences: **what** changed and **why**. No per-file inventory. No implementation tour.

### 2. Changes

Bullets only. Every meaningful **added** or **changed** behavior/feature gets a bullet. Merge tiny related tweaks; don't list every touched path.

Good:

- `iris catalog list` falls back to embedded packs when no remote catalogs are configured
- Overlay install refuses non-leader with an inline banner

Bad:

- Updated `internal/foo/bar.go`
- Fixed stuff
- Various refactors

### 3. Evidence (required)

**CI checks are non-conclusive.** Green unit/integration/lint does not replace Evidence. Never use `go test` / `go build` / CI status as the Evidence section.

Pick evidence that proves **this PR’s claim**:

| PR kind | Valid evidence |
|---------|----------------|
| Behavior / CLI / engine | TUI or CLI copy-paste (prompt + command + relevant output) |
| UI | Screenshot / print attached to the PR |
| **Layout / package move / folder structure** | **Directory tree** of the new shape (and a one-line note of what left the old place) |

Prefer TUI + CLI paste together when the change is visible in the TUI **and** has a headless command path.

Evidence must match **this PR's** result (not an old run or another branch). Redact secrets. Trim noise; keep enough that a reviewer can trust the outcome.

Not sufficient alone:

- “`go test ./...` passed”
- “CI green”
- “LGTM locally” with no output
- Logs with no command or context
- Test output offered as proof of a layout change

### 4. Notes (optional)

Out of scope, follow-ups, `Refs #` / `Closes #`. **Delete the section** if empty.

## Title

Keep the title specific and compact (issue id when applicable). Body carries detail; the title is the one-line hook.

## Workflow

1. Draft Summary + Changes from the actual diff (not from memory of the plan).
2. Capture Evidence that matches the PR kind (tree for layout; TUI/CLI/screenshot for behavior).
3. Fill `.github/PULL_REQUEST_TEMPLATE.md` sections; remove unused Notes.
4. Open or edit the PR. Completion criterion: body has non-empty Summary, Changes with real bullets, and Evidence that proves the claim (not CI).

```bash
# Example: open PR with body file
gh pr create --base development --title "<compact title>" --body-file /tmp/pr-body.md
```

## Common Pitfalls

1. **CI as Evidence** — tests/lint belong in your private checklist; they do not fill the Evidence section.
2. **Wrong evidence kind** — layout PRs need a folder tree, not test output; behavior PRs need runtime paste/screenshot, not only a path list.
3. **File laundry lists** — path lists are not Changes; describe user/engine-visible behavior (except tree evidence, which is intentionally structural).
4. **Empty template leftovers** — don't leave placeholder bullets or an empty Notes stub.
5. **Stale paste** — evidence from another branch or pre-fix run misleads reviewers.
6. **Wall of text** — if Summary exceeds ~4 sentences, move detail into Changes bullets or cut it.

## Verification Checklist

- [ ] Summary is 2–4 sentences (what + why)
- [ ] Changes lists every meaningful add/behavior change as bullets
- [ ] Evidence matches PR kind (tree for layout; TUI/CLI/screenshot for behavior) — not CI/test-only
- [ ] Evidence corresponds to this PR's commits
- [ ] Notes omitted or useful; issue links present when relevant
- [ ] No “Done when” / acceptance-checklist section (not used in this repo’s PR body)
