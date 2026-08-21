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

## What Lambda does not sell

**No reclaimable tier.** No bid, spot, preemptible or interruptible concept
anywhere in the API. A `RECLAIM_POLICY_PREFERRED` request therefore returns
nothing rather than quoting the on-demand price: an operator who asked for
reclaimable capacity asked for a discount, and silently billing them full price
answers a question they did not ask.

**No VRAM per card.** `specs` carries `vcpus`, `memory_gib`, `storage_gib` and
`gpus`, and never the card's memory. So a Lambda candidate mixes live price with
catalogued VRAM, and an uncatalogued shape reports 0 rather than a guess.

**No API-creatable filesystems.** `POST /file-systems` returns 405; persistent
filesystems exist and are dashboard-only. This is why Lambda has no
`VolumeManager` and why the warm-cache path cannot reach it. See
`docs/design/0004-cross-provider-warm-cache.md`.

## min_ram_gb is unenforced here

Lambda publishes no system-RAM figure in `skus.go`, so the shared resolver's
"a fact the provider does not publish cannot filter" rule leaves `min_ram_gb`
alone, and the rent path never consults `/instance-types` to check it. So the
requirement is accepted and not applied.

That is a real hole rather than a design choice, and it is worth knowing before
someone relies on the flag. `/instance-types` carries a real `memory_gib` per
shape, so the fix is available; it needs a network call on the rent path, which
is why it has not been taken yet.
