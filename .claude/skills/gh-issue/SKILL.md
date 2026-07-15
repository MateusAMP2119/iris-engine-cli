---
name: gh-issue
description: Create GitHub issues in the user's house style — compact, objective, evidence-first. Use whenever the user asks to file, draft, or open a GitHub issue.
---

# gh-issue — file GitHub issues in house style

Create the issue with `gh issue create --repo <owner/repo> --title <t> --body-file <f>`.
Write body to a scratchpad file first; show title + body to the user before submitting unless already told to just file it.

## Structure

Title: factual symptom statement, lowercase after first word, no period. States what is wrong, optionally the false claim the tool makes. Example: `engine uninstall leaves the managed Postgres tree (.iris/pg) and logs behind, reports "no on-disk engine state to remove"`.

Body sections, in order (omit a section only when genuinely empty):

```
## Summary
## Reproduce
## Cause        (only when root cause is known; cite file + function)
## Expected
## Notes        (version, platform, related issues)
```

- **Summary**: what is broken and why it matters. 2–4 lines.
- **Reproduce**: numbered steps, exact commands, quoted output in fenced blocks. Real observed output, never paraphrased.
- **Cause**: file paths and function names in backticks (`engineUninstall` in `internal/cli/engine.go`). State what the code does vs what it skips.
- **Expected**: correct behavior, plus acceptable minimum if full fix is large.
- **Notes**: version observed, OS/arch, `Related: #NNN` links.

## Voice — compact, objective

Caveman-compressed prose: drop articles and filler where removal loses nothing; fragments fine when they carry same info. Keep articles when dropping them hurts precision. Technical substance never compressed: commands, output, paths, identifiers, version strings stay exact and complete.

Hard rules:
- **Never address a person.** No "you", "your", "we", "I". Subject is always the tool, command, code, or user-as-role ("running the tour fails", not "when you run the tour").
- **No dashes as punctuation.** No `--`, no `—`. Use commas, colons, semicolons, parentheses. Hyphens inside words and CLI flags (`--yes`) are fine.
- Present tense for behavior ("check stats socket, never dials it").
- Every claim backed by evidence: quoted output, code reference, or repro step. No speculation without labeling it.
- No praise, apology, hedging, or narrative ("interestingly", "unfortunately", "it seems").

## Calibration example

Wrong:

> When you run `iris engine status`, you'll see "running" — but unfortunately the process is actually dead.

Right:

> ## Summary
> `iris engine status` reports "running" when socket file exists but process is dead. Check in `internal/cli/engine.go` stats socket, never dials it.
>
> ## Reproduce
> 1. `iris engine start` in `~/iris-demo`
> 2. `kill -9 <daemon pid>`
> 3. `iris engine status` → `engine: running (socket .iris/daemon.sock)`
>
> ## Cause
> `engineStatus` only checks socket existence. Stale socket after crash passes check. No dial, no ping.
>
> ## Expected
> Status dials socket. Dead process → "not running, stale socket found". Nonzero exit.
>
> ## Notes
> Observed v0.5.2, darwin/arm64. Related: #170.
