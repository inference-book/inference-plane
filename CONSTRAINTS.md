# Architectural Constraints

This file holds the enforceable architectural rules for `inference-plane`.
Each rule names what it constrains, why the project carries it, and how to
verify the constraint mechanically. The set is meant to stay small;
constraints are extracted from real friction (a wrong-direction PR, a
review that surfaced a hidden coupling), not invented speculatively.

See `ARCHITECTURE.md` for the design these constraints protect.

`make check-constraints` runs every verify line. The intent is that local
dev and CI (when the workflow exists) both gate on it.

---

## CP/DP-1: Data plane reaches control plane only via gRPC

Data-plane components (the router today; future per-cluster routers,
per-tenant routers, edge proxies) reach control-plane state only
through the generated gRPC client interfaces in
`gen/go/.../*connect/`. Direct imports of `internal/provisioners`,
shared `*provisioners.Service` pointers, or shared in-memory state
are not permitted from data-plane packages.

In `iplane serve` the data plane uses the in-process loopback gRPC
client (already used by v0.1's HTTP gateway; see ARCHITECTURE.md
"in-process gRPC"). In a future split where the data plane runs in a
separate process or per-model cluster, the same wire contract works
unchanged -- that is the point.

**Why we carry this.** The v0.2 daemon hosts both the control plane
(state-of-record, lifecycle loops, RPC handlers) and the data plane
(router) in one process for teaching reasons -- the Ch 7 chapter's
title is "Routing, Queueing, and the Control Plane in the Data Path."
But the long-arc design has the data plane splitting out eventually
(per-cluster routers, per-tenant edge proxies, the v1.0
multi-operator-sync story). Letting the router import
`internal/provisioners` directly would couple the two planes through
shared Go state, and the eventual split would require a refactor
across the whole router code base. Enforcing the gRPC-only boundary
from day one makes the split mechanical: run two binaries, change
the dial address, done.

**Why it is not "just a code-review rule".** Import-graph violations
are easy to introduce by reflex (auto-import, copy-paste from a
sibling package) and easy to miss in review (the import is one line
among many). A CI grep is cheap and catches the regression at the
edit-time level rather than the review-time level.

**Out of scope for this constraint.** Control-plane code reaching
its own state directly is fine -- the constraint is one-way. CLI
verbs (which are themselves gRPC clients) reaching the daemon via
the same client interface is the expected pattern, not a violation.

**Verify:**

```sh
test -z "$(grep -rln '"github.com/inference-book/inference-plane/internal/provisioners"' internal/router internal/dataplane 2>/dev/null)"
```

The match-content check (`test -z`) is used in place of the more
obvious `! grep ...` because BSD grep on macOS returns exit 2 when
one of the search paths does not exist -- which is the v0.2 norm
until the router lands. Exit 2 inverted by `!` would silently pass
a real violation. Checking the captured stdout is fast and
robust to absent directories.

`make check-constraints` runs this and exits non-zero on any match.
`tests/constraints/cpdp1_test.go` cross-checks the gate against a
synthesized violation, against a sibling-package non-violation, and
against the all-dirs-missing baseline so the constraint cannot
quietly stop working when the import path or grep behavior drifts.

---

## Adding a new constraint

Open a PR that:

1. Adds a numbered section above (CP/DP-2, CP/DP-3, ...) with the
   same shape: rule, why we carry it, why it is mechanical not a
   review rule, out of scope, verify.
2. Adds the verify line to `make check-constraints`.
3. Adds a paired Go test under `tests/constraints/` that runs the
   verify against a synthetic violation.
4. References the originating incident or PR review in the body so
   future readers can find the why.

Follow the "extract when a second instance appears" pattern from
`ARCHITECTURE.md` -- the goal is a small set of rules each pulled
from a concrete event, not a comprehensive style guide.
