# internal/stores/

Storage and concurrency primitives that grow up alongside the v0.2+
control plane. Each subpackage owns one shape; impls live in sibling
subdirs so a second backend can land without touching the interface
file.

Provisioner state already has its own storage tier at
`internal/provisioners/stores/{file}/` from Beat 1 (PR 112). That tier
is intentionally local to the provisioner domain — record persistence
for `Instance` / `Deployment`. This umbrella is for cross-domain
primitives that the router, scheduler, queue, and future per-replica
state all share.

## Subpackages

- **`queue/`** — bounded FIFO + M/M/k worker pool. The Beat 2 queue
  + scheduler lands on top of this; the chapter teaches it as the
  queueing-theory shape (k servicers, bounded waiting room, fail-fast
  backpressure).

## Conventions

- One Go interface per subpackage at the top level (`queue.go`,
  `<thing>.go`); impls in sibling subdirs (`queue/inmem/`,
  `queue/redis/` when it arrives).
- Generics over the payload type (`T any`). Subpackages do not
  reference any iplane-specific types — they're reusable primitives
  scoped to this repo for now; the queue + pool are slated for
  pushdown to `github.com/panyam/gocurrent` once their API stabilizes.

## See also

- [ARCHITECTURE.md](../../ARCHITECTURE.md) "What's deferred" → stores
  tier landed in v0.2 Beat 1 (provisioner) and Beat 2 (queue).
- [CONSTRAINTS.md](../../CONSTRAINTS.md) CP/DP-1 — data-plane code
  reaches control-plane state via gRPC. Stores in *this* umbrella
  are sub-process primitives, not control-plane state, so the
  constraint doesn't apply directly. Constraint surfaces only when a
  store would persist control-plane records.
