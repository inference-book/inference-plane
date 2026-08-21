# provisioners notes

Lore for `internal/provisioners`: the shared deploy path, the capability seams
the adapters opt into, and the accounting that hangs off an instance.
Per-provider lore lives in the adapters' own NOTES: [vast](vast/NOTES.md),
[runpod](runpod/NOTES.md), [lambdalabs](lambdalabs/NOTES.md). The catalog
resolver documents its own rules in `skucatalog/skucatalog.go`.

## Real deploys need three timeouts aligned, not one

A cold RunPod deploy is image-pull-bound (~10 GB engine image on community
capacity) and routinely runs 4-12 min, so: (a) the CLI `deployment deploy
--timeout` (client ctx, default 8m) must cover it; (b) the daemon's
`server.write_timeout_sec` (default 600) must exceed the wait or
`CreateDeployment` severs with `unexpected EOF` mid-provision; (c) the
provider engine-`/health` wait (`runpod.WithEngineReadyTimeout`, default 10m,
now wired to `IPLANE_RUNPOD_ENGINE_READY_TIMEOUT`) must not fail a
slow-but-fine pull; and (d) the **container disk** (`--min-disk-gb`) must fit
image + downloaded weights or a big-model cold deploy fills the disk
mid-download and fails. The RunPod deployer sizes `ContainerDiskInGB` from
`min_disk_gb` (was hardcoded 20 GB); `min_disk_gb` now **sizes the container
disk and no longer filters SKUs** (`DefaultDiskGb` was never a per-SKU ceiling,
since disk is an independent create param).
`examples/08-scaling-30b/08a-cold-start-distance/` sets the three timeouts
(20-45m) and `--min-disk-gb` (150 for a 72B FP8). Cross-cutting: the 72B FP8
cold-start run needed a **Blackwell-capable vLLM image**
(`vllm/vllm-openai:v0.26.0-cu129`, not the demo default `v0.7.0`) because the
only available FP8 card was a B200; FP8 needs Hopper/Ada/Blackwell.

## The engine-readiness loop is shared, and what a tick can see is not

`internal/provisioners/enginewait` owns the poll loop the image-native
providers run between renting a box and the engine answering `/health`:
deadline, monotonic phase ladder, per-tick emit, and the `Fatal` path that
lets a provider stop billing a dead host. Each adapter keeps an `Observe`
callback, because RunPod knows its endpoint up front and asks a second API for
the phase while Vast discovers the endpoint by polling and reads the phase off
the record it already fetched. `Config.Endpoint` is what encodes that
difference: seeded means probe before observing, so an already-serving pod
costs no status read. **The sshdocker executor deliberately stays out** (curl
over SSH, caller-owned deadline, no endpoint) since it shares the name of the
problem rather than the loop. Extracted after the terminal-failure guard was
added to one copy and never reached the other (#268).

## "Is this instance dead?" is a Provider capability, and providers differ on whether they can answer

`provisioners.FailureReporter` (optional, asserted like `KeyRegistrar` /
`SSHReadyWaiter` / `VolumeManager`) lets a readiness wait stop early instead
of billing the whole engine-ready timeout; `provisioners.TerminalFailure` is
the guard that makes "no capability" mean "keep waiting" in exactly one place.
Vast implements it; **RunPod deliberately does not**, for a measured reason
(see its NOTES.md). Whether a provider can answer is a property of its API,
not a gap to fill.

## Cost is measured from the instance, never asserted by the daemon

`CostRecorder` used to stamp one `{provider, gpu_type, billing_mode}` tuple,
built at startup from the operator's shell, onto every emission, and
`observeUptime` measured from daemon start rather than from anything billing.
Both were v0.1 holdovers: one daemon now runs many deployments and a
heterogeneous fleet spans providers inside one of them (#163). Now
`provisioners.Service` implements `metrics.FleetSource`,
`instance.uptime.seconds.total` emits one series per rented instance from its
**`created_at`**, and `instance.rate.usd_per_second` carries the price the
provider quoted at spawn. **Spend is a join on `instance_id`**, not a
projection: `sum(instance_uptime_seconds_total * on(instance_id)
instance_rate_usd_per_second)`. The router labels
`inference.active.seconds.total` with the replica that served the request and
nothing else, because it holds the id for free and a control-plane hop per
request already cost a 25s p95 once; enrichment happens where the data lives.
**Zero rate means unknown, not free**, so an unpriced instance is omitted from
the rate gauge rather than summed as zero. **Bill from `created_at`, not
`activated_at` (#335).** On an image-native provider the instance IS the
engine pod, so `activated_at` lands only when the deploy reaches RUNNING and
the whole cold start reads as free: a real 72B run reported $0.18 against an
actual $1.89. A PENDING instance is billing, because that is where an
image-native cold start spends all of its time. `billing_mode` derives from
the provider and is correct today, because **no rental is ever reclaimable**:
`reclaim_policy` constrains selection only, and nothing at rent time reads it
(#333).

## A tolerant filter figure and an exact accounting figure are two fields, not one

`Candidate.vram_gb_per_gpu` is what the provider advertises, and the three
adapters disagree by a gigabyte on the same physical card because a
marketplace of self-reporting hosts needs a floor generous enough not to
reject good machines (#283). `vram_bytes_per_gpu` is what the card holds,
resolved from the catalog by `skucatalog.ExactVRAMBytes` rather than from any
vendor's self-report. The two want opposite error directions: a filter should
be generous so it does not reject, a budget conservative so it does not
promise a fit that OOMs on arrival. Retuning the shared number would have
re-broken the filtering #283 fixed. **A vendor's "80GB" label is 80 GiB**, so
it holds 85.9 decimal GB; reading it as decimal took seven percent off every
card, and `iplane model budget --vram-gb` did exactly that until #323. Labels
nobody has verified as binary counts (H200's 141, Blackwell's 180 and 192)
resolve to unknown rather than to a number that is wrong by gigabytes on the
priciest cards. The B300 is the measured case: it reports 275040 MiB against a
"288GB" label, so the label is decimal and reading it as binary would promise
309 GB per card (#401). The exact figure reaches a deploy through
`provisioners.CardCapacityReporter` (optional, asserted like
`FailureReporter`), because the pre-rent check runs synchronously in
`CreateDeployment` and placement, the only other place a resolved `Candidate`
exists, runs asynchronously after the RPC has returned.

## External is a non-owning provider, and its hollow lifecycle methods are deliberate

`internal/provisioners/external` registers a RUNNING deployment pointing at an
operator-managed engine URL (no provisioning). `Terminate` detaches (never
destroys the engine), `Describe`/`List` are empty, `Spawn`/`Deploy` fabricate
from the endpoint. It skips the GPU-requirement gate, the image requirement,
and model validation (nothing to fetch). Don't "fix" the empty methods. See
its package doc; the hosted-API provider (Ch 11, issue 182) is external + auth
+ per-token cost.

## A provider panic used to take the daemon with it

`launchReplica` runs a provider's `Deploy` in a bare goroutine, so an
unrecovered panic inside an adapter is a process-level crash rather than a
failed replica. The Vast deployer did exactly that (#392) and killed
`iplane serve` mid-deploy.

The reason it is contained now is money rather than tidiness. That panic
landed at `vast:find-offer`, before anything was rented, so it cost nothing.
Twelve lines further down the same function is `rentEngine`. A panic past that
point kills the control plane while it holds a rented machine whose contract
id never reached the state file, so nothing knows the box exists: no teardown,
no destroy target, no record to reconcile. At an eight-card shape that is $10
an hour billing to an account nobody is watching.

`runReplicaDeploy` carries the recover and returns a named error, which is
what keeps the send on `results` in exactly one place: the aggregator counts
one result per slot, so a double send miscounts successes and a missed send
hangs `CreateDeployment` forever. The failure reason says "panicked" on
purpose, since "no capacity" and "the adapter crashed" send an operator to
different places (#393).

## The rate is stamped when the machine is rented, not when it serves

An instance created by a deploy used to learn its hourly rate from the
`Describe` inside `finalizeInstanceAfterDeploy`, and that only runs when the
deploy succeeds. So a deploy that rented a machine and then failed recorded no
rate at all, and zero means unknown, so the rental was dropped from
`instance.rate.usd_per_second` rather than priced wrongly. Spend is a join on
`instance_id`, so the box vanished from it silently: no zero in a graph to
notice, just one fewer series.

The failures that hides are the expensive ones. A deploy that fails instantly
costs nothing and records nothing, correctly. One that fails after forty
minutes on eight cards costs real money and recorded the same nothing: a
measured 35-minute 8x A100 rental read `$0.0000/hr` (#397).

`DeployStateUpdate.HourlyRateUSD` is the seam. Vast reports `offer.DphTotal`
at the moment it rents, which is the only moment anything sees that number,
since the offer leaves the marketplace seconds later. RunPod binds
`costPerHr` off the create response. Both patch paths stamp the instance the
way they already stamp `provider_id` from `ContainerID`; the single-instance
one keys on the deployment's **instance id**, because that coincides with
the deployment id only for an auto-provisioned 1:1 record and a deployment
placed onto an existing instance otherwise prices nothing.

## A member spanning nodes is all-or-nothing, and the fan-out around it is not

`ReplicaSpec.nodes` above 1 makes each member of that group rent several
machines and serve on one endpoint (#212). The two rules look
contradictory next to each other and both are right:

- **Across members**, a partial result is a service. Three of five
  independent replicas is DEGRADED and still answering, so the fan-out
  keeps the survivors.
- **Within a member**, a partial result is nothing. Three of four nodes
  is not three-quarters of an engine, so `launchMember` hands the whole
  span back and reports one failure.

The money is why the second rule exists rather than being a nicety. On an
image-native provider the machine is rented inside `Deploy`, so a member
that lost one node is holding N-1 live rentals against an engine that
never started, and nothing else would ever return them.

**The image goes onto every node.** That is how a multi-node engine runs:
the same container everywhere, ranks finding each other through the
arguments the operator passed, rank 0 serving. iplane still does not
decide how they assemble; it puts the image on the machines and lets
`engine_args` say the rest, which is the same boundary the engine-args
pass-through has always drawn.

**Only rank 0 names the member's endpoint.** Every node reports one and
nothing routes to the workers, so `patchDeploymentSlot` takes the
endpoint only from the member's first instance. Without that the last
writer wins and traffic goes to a worker.

**`returnMember` is best-effort by necessity.** It calls `DestroyInstance`
per machine and logs what it cannot return; there is nothing to escalate
to. A machine whose provider id was never recorded cannot be terminated
through the API at all, which is the same hole #161 describes from the
other side, and the instance record is deliberately left behind so an
operator has something to act on.
