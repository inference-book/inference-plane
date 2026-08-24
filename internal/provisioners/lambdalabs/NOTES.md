# Lambda Labs adapter notes

Implementation lore for `internal/provisioners/lambdalabs`. The package doc
covers the wire format (HTTP Basic rather than Bearer, the fixed instance-type
catalog); this file is what the API turns out to be like once you ask it
questions the catalog cannot answer.

Lambda is the **fixed-catalog** adapter, and the useful way to hold it is as
the opposite of Vast. Vast has live offers from independent hosts that vary in
everything. Lambda has a short list of shapes at published prices, where the
only thing that varies is which regions currently have any.

## /api/v1/instance-types is richer than the static catalog

`skus.go` carries a curated subset with hand-recorded prices. The live endpoint
carries more, and three fields on it are worth knowing about.

**`regions_with_capacity_available`** is the availability signal and there is
nothing else like it. Probing live on 2026-08-15, **fifteen of twenty-three
shapes had capacity in no region at all**. A static catalog cannot express that
and neither can a dry run, which is most of why `Candidates` makes the call.
Re-read on 2026-08-20 it was six of twenty-two, and the two eight-card A100
shapes were among them, so the answer moves by the day.

**`price_cents_per_hour`** is authoritative and the catalog's figure is not.
They agreed when checked, and the live one is what `Candidates` reports.

**`architecture`** was the surprise. Lambda's GH200 shapes are `arm64` and
everything else is `x86_64`. An engine image built for x86 will not run on one,
and nothing else in the deployment path would have told you before the container
failed to start. This is why `Candidate.Architecture` exists as a typed field
rather than a provider attr: Vast reports the same fact as `amd64`, so it is one
fact with two spellings and belongs normalized.

The response is **keyed by instance-type name**, not a list. Decoding it as a
list yields an empty map, which reads exactly like "no capacity anywhere", so
`TestInstanceTypesDecodeIsKeyedByName` pins the shape against a fixture copied
from a live response.

## The catalog is a transcription, and it was wrong about a card

`skus.go` is hand-copied from `/instance-types`, and until #380 nothing
checked the copy. Two rows had drifted from the vendor's own answer.

`gpu_1x_a100_sxm4` carried 80 GB against a shape Lambda calls **"1x A100
(40 GB SXM4)"**. The 80 GB SXM4 part exists on Lambda only as
`gpu_8x_a100_80gb_sxm4`, the eight-card box. The wrong figure reached
`CardCapacityBytes`, so the pre-rent budget check was promising 85.9 GB
per card on a card holding 42.9, which is the over-promise direction that
runs out of memory after the meter has started. `gpu_1x_a100`'s price was
also stale, $1.29 recorded against $1.99 live.

So `testdata/instance-types.json` is a recorded response (2026-08-20,
trimmed to the fields the catalog transcribes) and
`TestCatalogTranscribesTheVendorsInstanceTypes` checks every row against
it: display name, card count, price, and the card memory parsed out of
`gpu_description`. Refreshing the catalog means re-recording the fixture
and moving both together, which is the point.

**A consequence worth knowing.** With the A100 SXM4 correctly at 40 GB,
the cheapest Lambda shape clearing 80 GB is the GH200, and the GH200 is
arm64. A one-card `class: large` request therefore resolves an arm64 box
first, and an x86 engine image will not start on it. Nothing in
`ResourceRequirements` carries architecture, so the resolver cannot filter
on it yet; `Candidate.Architecture` reports the fact and an operator has
to read it.

## Multi-GPU shapes, and the two that stay out

Lambda is the one provider whose catalog rows describe a whole instance
rather than a card, so `skucatalog.Match` filters on `Entry.GPUCount`.
Every row used to be `GPUCount: 1`, which meant a request for eight cards
was excluded before the adapter called Lambda at all, and `iplane capacity
--gpu-count 8` reported nothing while RunPod and Vast both answered
(#380). The 2x, 4x and 8x shapes are now catalogued.

The form factor is in the token and the description for almost every
shape, which is what makes the fabric family readable without a
measurement. The SXM4, SXM5 and SXM6 rows carry a board-integrated fabric
and resolve `INTRA_NODE` from the declared tier. The PCIe A100s and the
A6000s are bridge-capable, so under `FabricDeclared` they resolve UNKNOWN
and drop out of a fabric request. That is the doctrine working: Lambda
reports no measurement, and renting one to find out costs money.

Two shapes are left out for want of evidence rather than interest.
`gpu_8x_v100` and `gpu_8x_v100_n` are both described as "Tesla V100 (16
GB)" with no form factor anywhere, and the SXM2 part has NVLink while the
PCIe part has none, so either mapping asserts something nobody vouched
for. `gpu_1x_rtx6000` is the 24 GB Turing Quadro, not the 48 GB RTX 6000
Ada that `FamilyRTX6000Ada` names. Both stay rentable through an explicit
`--gpu-sku`.

## The key the VM boots with, and the key iplane holds

Until #427 this adapter had no `KeyRegistrar`, and the package doc said it
did. The Service skips registration for a provider that is not one, so
iplane's own key never reached the account. `Spawn` then attached whatever
key `/ssh-keys` listed first, which on a real account is the operator's own,
and the deploy path handed `sshdocker` the private half of a key iplane had
generated locally. The two were never the same key. **The deploy path could
not have worked on hardware, and no unit test could see it**, because each
half is correct on its own and only the pair is wrong.

`keyregistrar.go` closes it. Lambda's key model is named objects rather than
RunPod's single `authorized_keys` blob, so the shape is list, compare,
replace, and the comparison is on parsed key material so a differing comment
is not a differing key.

**The stored name deliberately drops the timestamp.** iplane's key comment is
`iplane-<operator>-<provider>-<rfc3339>`, and keeping the timestamp would
give every regenerated keypair a new name, so a wiped keystore would leave
the account accumulating `iplane-` keys with no way to tell which one the
current private half matches. The derived name is stable per operator and
provider, which turns re-registration into a replace. Lambda bounds the field
at 1..64 characters, so it is sanitized and truncated.

`Spawn` prefers the `iplane-` key and falls back to the account's first key,
which keeps an operator driving the adapter by hand working the way it always
did.

## The SSH readiness gap was measured and then not waited for

`WithSSHReadyWait` and `WithSSHProbe` have been on the Provider since the
adapter landed, along with a comment putting Lambda's gap at "usually under
60s". Nothing read either field: there was no `WaitForSSHReady`, so Lambda
did not satisfy `SSHReadyWaiter` and the Service's wait was a no-op. Whatever
address `Spawn`'s post-launch describe happened to return went straight to
the executor, and Lambda's describe can answer with a booting instance and an
empty `ip`.

`sshready.go` implements it as two stages under one deadline, matching
runpod's: poll describe until `ip` is non-empty, then probe the port, because
an assigned address says the VM exists and says nothing about sshd. Both
stages share `sshReadyTimeout` so the caller waits one budget rather than two
that compound.

## Ownership moved onto the tags, and the name stayed

The adapter stamped `name` as `iplane-<id>` and read it back by prefix, which
made ownership depend on a display field an operator can change from the
console. `withSystemTags` has always put `iplane-id` and `iplane-operator` on
the Spec before every Spawn and this adapter dropped them.

Lambda's launch call takes `tags` directly, so there was never a second API
call to justify the omission. Both are stamped now, and `List` prefers the
tags where both answer, because a rental made before this carries only the
name and the name may since have been changed.

**A tag key outside `^[a-z][a-z0-9-:]+$` fails the whole launch with a 400**,
so `launchTags` drops a key the vendor would reject rather than refusing to
rent. A tag is bookkeeping and the launch is what the operator asked for;
failing the expensive half over the cheap one is the wrong trade. The two keys
iplane stamps itself always pass. `hack/lambda-watchdog.sh` claims an instance
by either signal for the same reason the adapter reads both.

## The status vocabulary, checked against the vendor rather than against us

Lambda publishes six instance statuses and the adapter handled five.
`preempted` fell through to the unknown-value default, which is `PENDING`,
and `PENDING` is the one answer that costs something: the caller waits out
the whole engine-ready deadline on a machine that is gone. Lambda sells no
reclaimable tier, so a preemption is the vendor taking the box back rather
than anything an operator asked for.

`testdata/openapi-shapes.json` is the vendor's own OpenAPI document trimmed
to the shapes this adapter decodes, recorded 2026-08-24 against API version
1.10.0. Two tests read it. One checks that every `json` tag the adapter
declares names a field Lambda actually publishes, because a tag that does not
decodes to the zero value on every response and **a fabricated zero looks
exactly like a measurement**. The other checks the status mapping against the
published enum in both directions. Refreshing means re-recording the fixture
and moving the code with it.

Worth knowing from the same reading: the instance record carries a
first-class `tags` array, which would be a better ownership signal than the
`name` field this adapter stamps, and it carries no timestamp of any kind,
which is why `hack/lambda-watchdog.sh` ages an instance from first sight.

## What Lambda does not sell

**No reclaimable tier.** No bid, spot, preemptible or interruptible concept
anywhere in the API. A `RECLAIM_POLICY_PREFERRED` request therefore returns
nothing rather than quoting the on-demand price: an operator who asked for
reclaimable capacity asked for a discount, and silently billing them full price
answers a question they did not ask.

**No VRAM per card.** `specs` carries `vcpus`, `memory_gib`, `storage_gib` and
`gpus`, and never the card's memory. So a Lambda candidate mixes live price with
catalogued VRAM, and an uncatalogued shape reports 0 rather than a guess.

**No API-provisionable clusters.** 1-Click Clusters are the cross-node,
InfiniBand product, and they are console-only: every cluster-shaped v1 path
404s (`/clusters`, `/cluster-types`, `/one-click-clusters`) while the paths
this adapter uses answer normally. Lambda's docs describe a wizard, a
reservation stated **in weeks**, and an invoice. So it is neither
API-provisionable nor on-demand, and a weekly invoice is a different
commercial shape from the per-second rentals the cost model is built on.
Checked 2026-08-21; see the comment on #352.

**Filesystems ARE API-creatable, and the four weeks we believed otherwise are
worth the paragraph.** `docs/design/0004` recorded that `POST /file-systems`
returns 405 and concluded Lambda has no API-creatable filesystems, which is
why this adapter has no `VolumeManager` and why the warm-cache path cannot
reach it.

Lambda spells the collection two ways. `GET /api/v1/file-systems` is
hyphenated and `POST /api/v1/filesystems` is not, and only the second takes a
create. The 405 was fired at the read path, and the wrong conclusion held
because a 405 is a confident-sounding answer: it says the method is not
allowed here, which reads as "the vendor does not offer this" rather than
"you knocked on the wrong door".

Probed live 2026-08-24. Creating and deleting a filesystem both work, the
record carries `mount_point: /lambda/nfs/<name>` derived from the name, and a
filesystem exists independently of any instance. `file_system_names` is
accepted by the launch call and validated **before** capacity is checked, so
a wrong volume name costs an error rather than a billed box that mounts
nothing. Region-locking is the vendor's own rule and its error names both the
filesystem and the region.

What is still unverified is whether the mount appears inside the engine
container at that path, which needs a rented box and is folded into #427.

The general lesson is the one worth keeping: **a 4xx tells you about the
request, not about the vendor.** Reading a 405 as a capability statement is
how a whole feature stayed written off. Check the published path list before
concluding an API does not do something.

## min_ram_gb is unenforced here

Lambda publishes no system-RAM figure in `skus.go`, so the shared resolver's
"a fact the provider does not publish cannot filter" rule leaves `min_ram_gb`
alone, and the rent path never consults `/instance-types` to check it. So the
requirement is accepted and not applied.

That is a real hole rather than a design choice, and it is worth knowing before
someone relies on the flag. `/instance-types` carries a real `memory_gib` per
shape, so the fix is available; it needs a network call on the rent path, which
is why it has not been taken yet.
