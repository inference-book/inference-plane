# Release lifecycle

This repo is paired with `../book/` in the Tanenbaum/MINIX model. Each
book version (`v0.1`, `v0.2`, `v0.3`, `v1.0`) maps to a chapter range
and to a release branch + tag in this repo:

| Book version | Chapters    | Branch          | Initial tag |
| ------------ | ----------- | --------------- | ----------- |
| v0.1         | Ch 6        | `release/v0.1`  | `v0.1.0`    |
| v0.2         | Ch 7–10     | `release/v0.2`  | `v0.2.0`    |
| v0.3         | Ch 12–15    | `release/v0.3`  | `v0.3.0`    |
| v1.0         | Ch 18–19    | `release/v1.0`  | `v1.0.0`    |

## While drafting a chapter

Work happens on `main`. The active release branch (the one paired with
the chapter currently being drafted) **stays a moving snapshot** —
`main` gets force-forwarded into it as new commits land, so the book's
`\cpbranch` always points at code that matches what the prose
describes.

Forward-merge: `git checkout release/v0.X && git merge main`.

After every forward-merge, **restore the release-branch pin labels**.
`pinned-versions.env` on `main` carries `CP_VERSION=dev` / `CP_BRANCH=main`;
the merge silently takes those values because the release branch
hasn't touched them since the common ancestor. Edit
`pinned-versions.env` back to `CP_VERSION=vX.Y.0` /
`CP_BRANCH=release/vX.Y` and amend the merge commit (or land a follow-up).

## When a chapter is done

Cut the release.

1. Verify `pinned-versions.env` on the release branch carries the
   correct `CP_VERSION` / `CP_BRANCH`.
2. Tag the release branch tip: `git tag vX.Y.0 release/vX.Y`.
3. Push the tag: `git push origin vX.Y.0`.
4. Stop forward-merging `main` into the now-cut release branch. The
   branch is a maintained errata channel, not a moving snapshot, from
   this point on.
5. The next chapter's release branch (`release/v(X.Y+1)`) is cut from
   `main` at this point and starts moving with the next chapter's work.

## Revisiting a finished chapter

Bugs and errata in a chapter whose release is already cut land on
`main` first, then **cherry-pick forward to every release branch from
the introducing chapter onward**.

- A Ch 6 fix → cherry-picks to `release/v0.1`, `release/v0.2`,
  `release/v0.3`, `release/v1.0`.
- A Ch 8 fix → cherry-picks to `release/v0.2`, `release/v0.3`,
  `release/v1.0` (v0.1 doesn't have Ch 8 content).
- Use `git cherry-pick -x <sha>` so the cherry-picked commit message
  records the original SHA for traceability.

After cherry-picking, **bump the patch tag** (`v0.1.1`, `v0.1.2`, …)
and update the book's `\cpversion` macro so readers tracking exact tags
get the fix. Never re-tag an existing version — readers who pinned to
`v0.1.0` must stay reproducible.

## Cherry-pick gotchas

- `pinned-versions.env`: same trap as the forward-merge — if the fix
  on `main` touched `CP_VERSION` / `CP_BRANCH`, the cherry-pick will
  flip the release branch's labels. Fix in the same commit (use
  `git cherry-pick --no-commit`, edit the env file, then `git commit`).
- `metric-names.tex` (in book repo) and `internal/telemetry/names.go`
  are paired generated artifacts; cherry-picks that change one must
  carry the other.
- `gen/` proto code is committed; cherry-picks that change `protos/`
  must carry the regenerated `gen/` files.
