---
name: publish-book-shift
description: Publish iplane architectural-shift markers to the paired book repo's inbox (`../book/iplane-inbox/`). Use when iplane has had meaningful architectural changes the chapter narrative needs to absorb — typically after a PR merges that changes vocabulary, package layering, capability boundaries, or operator-facing surface. First run dumps the current architectural state; subsequent runs are incremental since the last publish.
---

# publish-book-shift

You are helping the iplane author keep the paired book chapter (in `../book/`) in sync with iplane's architecture. iplane evolves faster than the chapter prose; this skill drops markers into a queue the book author drains on their own cadence.

The book repo and iplane repo are siblings on disk:

```
inference_is_all_you_need/
├─ inference_plane/   ← you are here
└─ book/              ← marker destination
   └─ iplane-inbox/   ← created if it doesn't exist
```

## Two modes

**First publish** (no `../book/iplane-inbox/.last-published-sha` exists):
- Dump the current architectural state. The book repo has never seen iplane; assume it knows nothing.
- Read `ROADMAP.md`, `ARCHITECTURE.md`, `README.md`, `docs/design/*.md`, recent merged PR commit messages (`git log --merges -20`) to map iplane's architectural surface.
- Produce 4-8 markers covering: provisioner abstraction, deployment model, CLI surface, demokit walkthroughs, state file + idempotency, telemetry vocabulary, anything else load-bearing.
- One marker per concept, not one per commit.

**Incremental** (`.last-published-sha` exists):
- Read the SHA. Walk `git log <SHA>..HEAD --merges` (or `--no-merges` if the repo has linear merges).
- Filter to commits that change architectural surface: anything touching `docs/design/`, `internal/provisioners/`, `internal/deployments/`, the proto, `cmd/iplane/cmd/`, the `examples/` walkthroughs, or `ROADMAP.md`. Skip pure-test commits, doc cleanups, dependency bumps.
- For each architectural shift, produce a marker. Multiple commits to the same concept collapse into one marker.

After writing markers, update `../book/iplane-inbox/.last-published-sha` with the current `git rev-parse HEAD`.

## Output format

Each marker is a markdown file at `../book/iplane-inbox/<ISO-timestamp>--<kebab-slug>.md`.

Filename:
- Timestamp prefix is `YYYY-MM-DDTHH-MM-SS` (colons are filesystem-unsafe; dashes sort the same way alphabetically)
- Slug describes the change ("image-as-pod-pivot", "wait-for-instance-ready-rpc", "demokit-fail-fast")

Body shape:

```markdown
---
title: One-line summary (matches the chapter-author's mental model, not the commit message)
iplane_pr: <#>            # null if not from a PR
iplane_commit: <full-sha> # the HEAD commit at publish time, OR the merge commit for incremental
date: YYYY-MM-DD
chapters: [chapter06]     # which chapter beats this affects
sections: [6.7, 6.8-deploy]  # specific sections by ID or descriptive slug
vocabulary_changes:
  - before: "Deploy = SSH + docker run on a base pod"
    after: "Deploy = run engine image as a pod (image-native) OR SSH+docker (VM-style, fallback)"
---

## What changed

2-4 paragraphs of the architectural delta. Cite specific files / PRs / line numbers where useful. The book author should be able to understand the shift without opening iplane.

## Narrative implication

What this means for the chapter's teaching arc. Where does the new concept slot in? Is there a manual-vs-iplane comparison to draw? Does it change which "act" of the chapter introduces this primitive?

## Suggested section work

Actionable checklist for the book author. Each item names a chapter file / section / outline beat:

- [ ] Update `master_outline.md` Chapter 6 beat X to mention Y
- [ ] Add a new subsection 6.N covering the Z primitive
- [ ] Rewrite `docs/design/0002-deploy.md` to mark old design superseded
- [ ] Search-and-replace vocabulary: "<before>" → "<after>" in chapter06 source
```

## How to identify architectural shifts (heuristics)

For each candidate commit / PR, check:

- **Proto changed?** (`protos/provisioner/v1/*.proto`) — the wire contract is the chapter's API surface. Always a shift.
- **Capability interface added/changed?** (`internal/provisioners/provider.go`'s `KeyRegistrar`, `SSHReadyWaiter`, `Deployer`, `DeploymentExecutor`) — these are the patterns the chapter teaches.
- **CLI verb added/removed?** (`cmd/iplane/cmd/*.go`) — the operator-facing surface readers will run.
- **Default behavior flipped?** (the chapter narrative depends on the default path; flipping it requires re-reading the chapter)
- **Demokit walkthrough restructured?** (`examples/provisioning/*-end-to-end/main.go`) — the chapter's reproduction story.
- **Design doc added or marked-superseded?** (`docs/design/*.md`) — explicit narrative shifts.
- **Vocabulary in `ROADMAP.md` or `ARCHITECTURE.md` rewritten?** — usually paired with a real shift.

Skip:
- Pure refactors that don't change behavior or shape
- Test additions
- Dependency bumps
- Bug fixes that don't shift the operator's mental model
- Internal layering changes that don't reach the chapter

When in doubt, err on side of including — a marker can always be skipped by the drain skill.

## Idempotency and re-runs

- If `../book/iplane-inbox/` doesn't exist, create it.
- If `.last-published-sha` is missing → first-publish mode.
- If `.last-published-sha` matches current HEAD and the user didn't pass `--force` → output "inbox is up to date at <sha>, nothing to publish" and exit.
- If the user passes `--full` or `--since <SHA>` (free-form text in the skill invocation), honor it.

## Conventions

- **Markers are immutable once written.** If you discover a marker was wrong, don't edit it in place — write a follow-up marker that corrects it. The drain skill processes in FIFO order; corrections after the fact are part of the history.
- **One concept per marker.** A PR that adds two unrelated things gets two markers, not one.
- **Reference specific files and PR numbers.** The chapter author may quote them.
- **Vocabulary changes use "before → after" pairs.** The drain skill can run them as a search-and-replace candidate.
- **Section IDs match the book's TODO.md structure** (e.g., 6.1, 6.7) when possible; descriptive slugs ("deploy-primitive") when no ID exists yet.

## End-of-run report

After writing markers, report to the user:

- N markers written, with one-line summaries
- Updated `.last-published-sha` to `<sha>`
- Suggested next step: "Run `/drain-iplane-inbox` in the book repo when you're ready to absorb these into chapter prose."

## What this skill does NOT do

- Does not write into the book chapter prose directly (the drain skill does that).
- Does not push to either repo (markers land in working tree; the book author commits on their cadence).
- Does not delete or modify existing markers (corrections go as follow-up markers).
- Does not invoke any other skill.
