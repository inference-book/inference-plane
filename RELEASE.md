# Release lifecycle

This repo is paired with `../book/` in the Tanenbaum/MINIX model. Each
book version (`v0.1`, `v0.2`, `v0.3`, `v1.0`) maps to a chapter range
and to a release branch + tag in this repo:

| Book version | Chapters    | Branch          | Initial tag |
| ------------ | ----------- | --------------- | ----------- |
| v0.1         | Ch 6        | `release/v0.1`  | `v0.1.0`    |
| v0.2         | Ch 7–10     | `release/v0.2`  | `v0.2.0`    |
| v0.3         | Ch 11–12    | `release/v0.3`  | `v0.3.0`    |
| v1.0         | Ch 13–16    | `release/v1.0`  | `v1.0.0`    |

There is no Part V row because the book no longer has a Part V. The 2026-08
restructure collapsed the old Part IV/V split into a single closing Part IV
of four chapters (Ch 13-16), so v1.0 now covers all of it. The SaaS-platform
material that used to be Ch 15-19 is one optional chapter, Ch 15, and stays
behind feature flags on `release/v1.0` since none of it is inference-specific.
See `ROADMAP.md`, which is canonical for the version-to-chapter mapping and
which this table must agree with.

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
3. Push the tag: `git push origin vX.Y.0`. This fires
   `.github/workflows/release.yml`, which cross-compiles
   `iplane-linux-{amd64,arm64}`, verifies the version stamp, and attaches
   the binaries plus `checksums.txt` to a GitHub Release.
4. Stop forward-merging `main` into the now-cut release branch. The
   branch is a maintained errata channel, not a moving snapshot, from
   this point on.
5. The next chapter's release branch (`release/v(X.Y+1)`) is cut from
   `main` at this point and starts moving with the next chapter's work.

**Why the release carries binaries.** The engine agent runs inside an
engine container on a rented box and gets there by being fetched from a
URL, so a published linux binary at a version-pinned address is a
prerequisite for that path rather than a convenience. `make dist`
produces the same artifacts locally; a build with no tag stamps `dev`,
and the delivery path reads `dev` as "no published artifact to fetch"
instead of guessing at a version that was never cut.

A publish that half-fails is recovered by re-running the workflow from
the Actions tab against the same tag: it overwrites the assets rather
than creating a second release. Never re-tag.

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
