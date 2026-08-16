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
