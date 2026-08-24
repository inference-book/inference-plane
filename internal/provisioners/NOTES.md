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

## Adopting an instance is riskier than reporting one, so it re-checks

`CreateInstance` asks the provider whether it already has an instance under
this id, and adopts the first ACTIVE ref it gets back rather than renting.
That lookup used to trust the adapter's filtering completely.

On a live Lambda run (#427) it adopted the wrong machine. Creating
`lambda-auto` while `lambda-probe` was running returned `AlreadyExisted` with
lambda-probe's provider id, and the state file ended up with two iplane ids
pointing at one machine while a second machine had no record at all.
Destroying either id would have terminated the wrong box and leaked the other.

The adapter fix in #431 stops the bad list, and this site now re-filters
locally as well, the same way the self-heal does. Two belts, for a reason that
is about consequence rather than tidiness: the self-heal reading a bad list
marks a record wrongly, and a later verb can still correct it. This one binds
a deployment to somebody else's machine and reports success, so nothing is
rented, nothing is corrected, and the operator has no signal at all.

## An adapter that drops a filter answers a question nobody asked

`Provider.List(ctx, filter)` is documented as match-all over tags, and it was
honoured by one adapter in three (#431). RunPod matched every key. Vast read
`label-prefix` and Lambda read `name-prefix`, and neither of those is a key
the Service ever passes, so both dropped the tag filter and returned the whole
account.

The Service then read the length of that answer as evidence. `ListInstances`'
self-heal calls `List({iplane-id: id})` and treats an empty result as "the
provider does not have this instance", which converges a record stuck in
TERMINATING. Against a lenient adapter the result is never empty, so the sweep
never converged. Against a strict one it was worse: the old call also passed
`iplane-operator`, which **no adapter can recover**, so every instance matched
nothing and every TERMINATING record was declared TERMINATED, alive or dead,
with nothing left to retry a terminate that had failed.

Three things changed and the third is the one that lasts.

`provisioners.MatchesTags` and `FilterRefs` are the one implementation, and
every adapter applies them over the tags it recovered. A filter key an adapter
cannot recover now excludes the instance. That is not a good answer, but it is
the shape of a real one: an ignored filter returns everything and reads as
confirmation, which cannot be told apart from a measurement.

The Service stopped asking a question no provider can answer. The self-heal
filters on `iplane-id` alone, and `--remote` passes no tag filter at all,
since the API key already scopes the account. Ownership is decided here
instead, on the one tag every adapter recovers: a ref with no `iplane-id` is a
box the operator rented themselves and is not rendered as an iplane Instance.
The self-heal also re-filters locally rather than trusting the adapter, which
is cheap and keeps the branch correct if an adapter regresses.

`internal/provisioners/providertest` is the conformance suite all three
adapter test packages call. It was written because every adapter's own tests
passed throughout: each was written against the filter that adapter happened
to implement, so nothing compared them. Each adapter declares the tags it can
genuinely recover and the suite builds its cases from that, which is how one
suite holds three providers with different wire formats to the same contract.

## Attaching a volume is a provisioning-time act for a VM-style provider

`Deployment.mounts` is provider-agnostic and each deploy path maps it onto
its own primitive, which worked while every provider attached a volume in
the same call that started the container. RunPod does: `networkVolumeId` on
pod-create. A VM-style provider cannot.

A Lambda filesystem is named in the **launch** request. The host directory
`/lambda/nfs/<name>` does not exist until it is, and the sshdocker executor
binds host paths when it starts the engine, minutes later. By then there is
nothing left to ask, so a mount that only reaches the deploy path is a mount
that silently never happens.

`Spec.volume_ids` carries the handles into provisioning, filtered by
`volumesForProvider` to the ones the replica's own provider issued, for the
same reason `checkMountProviders` exists: a handle means nothing to anyone
else, and a heterogeneous fleet carries one deployment-level mount across
replicas that need not share a provider. An image-native adapter never reads
the field.

`Volume.host_path` is the other half. The adapter records where its volume
lands at `EnsureVolume` time and `resolveWarmMount` carries it onto the
mount, so the shared path never learns that Lambda uses `/lambda/nfs`. Empty
for a provider whose volume never touches a host filesystem iplane can see.

## Destroy releases the container; Terminate releases the rental

Two calls that look like one, and the gap between them was a billing leak
(#161). `DestroyDeployment` runs `deployerFor(inst).Destroy` per replica.
For an image-native provider the deployer *is* the provider and the pod it
tears down is also the rental, so the two coincide. For a VM-style provider
the deployer is `sshdocker`, and all it did was stop a container over SSH:
the machine underneath is still rented, still billing, and the state file now
says TERMINATED. The operator sees a clean teardown and keeps paying.
`internal/provisioners/lambdalabs` is the adapter that shape applies to;
Vast stopped being affected when it went image-native in #252.

`releaseRental` closes it by calling `provider.Terminate` after a successful
`Destroy`, and it fires **for every provider rather than only the VM-style
ones**. Terminate is idempotent by contract, so the image-native second call
costs one request and returns success, and a future adapter whose Destroy
quietly does not release its machine leaks nothing. Making the call
conditional on a capability check would mean the leak comes back the moment
somebody's assumption about their own adapter is wrong.

That uniformity is load-bearing on the adapters holding up their end.
RunPod already mapped a 404 to success; Vast and Lambda both returned
`ErrNotFound` and now do the same. A terminate that fails for any other
reason is appended to `failures`, which leaves the deployment TERMINATING
with the reason attached rather than reporting a teardown over a live
machine. The reaper retries from there.

The ownership guard is the one `markInstanceTerminated` already carried: an
auto-provisioned replica exists only to back this deployment, while an
instance the operator placed by hand may be shared and is theirs to destroy.

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

## A multi-GPU engine can hang before it fetches a byte, and every signal reads fine

An eight-card B200 deploy sat in `engine:init` for half an hour. The
provider's status API said 188 MB on a 900 GB disk, 5.7 GB inbound (the
container image, not 474 GB of weights) and **86% GPU utilisation**. The last
engine log line, 23 minutes stale, was `vLLM is using nccl==2.30.7`.

Busy cards with a flat disk is a collective that never completed. vLLM
initialises NCCL across the ranks before loading weights, so a rendezvous that
hangs burns GPU in a busy-wait having fetched nothing. The failure belongs to
the parallelism rather than the model: a single-card deploy has no collective
to hang in.

What makes it nasty is that no individual signal looks wrong. `/health` not
answering reads as "still starting". 86% utilisation reads as "working". The
container is up and the process is alive. **Utilisation measures effort, not
progress**, and the only way to tell them apart is to watch something that
should be *changing*.

**Torch's NCCL watchdog does not catch this, and it is already on.** Verified
in `torch/csrc/distributed/c10d` at v2.13.0, which `vllm:v0.27.1` pins:
`TORCH_NCCL_ASYNC_ERROR_HANDLING` defaults to 3,
`TORCH_NCCL_ENABLE_MONITORING` to true, `TORCH_NCCL_HEARTBEAT_TIMEOUT_SEC` to
480. It watches collectives *in flight*, and initialisation has no work item
yet; NCCL communicators are created blocking by default, so init has no
timeout either. Setting any of those is cargo. Two of them were proposed here
before anyone checked.

Detection is the mitigation, and it lives outside the engine:
`hack/measure-run.sh` abandons a deploy whose cards stay busy while the disk
stays flat. `NCCL_DEBUG=INFO` (set by that script) makes the next one name the
stage it stopped at instead of going silent after one line.

## The provider already knows things nothing was asking it

The readings that diagnosed the hang were in the status response the deploy
path fetches every tick to advance its phase. Nothing read them.
`Instance.usage` (`ResourceUsage`) now carries disk used, GPU utilisation and
bytes received, filled by the Vast adapter from that same record.

The design point generalises past this bug: **prefer the observation with the
fewest preconditions.** The SSH path needed a routable address, a recorded
address, a registered key and a reachable box, and each missing one turned the
observation into silence rather than into an error. The provider's own API
needs none of them, and it answers on deploys where the shell cannot.

`available` on that message carries the same weight it does on
`InterconnectHealth` and `StagingProgress`. That is three signals now where
absent and zero had to be kept apart, which makes it a rule rather than a
coincidence: a fabricated zero looks exactly like a confident measurement.
