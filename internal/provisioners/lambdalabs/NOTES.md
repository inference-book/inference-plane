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

**A consequence worth knowing, and the reason two tickets exist.** With the
A100 SXM4 correctly at 40 GB, the cheapest Lambda shape clearing 80 GB is
the GH200, and the GH200 is arm64. A one-card `class: large` request
therefore resolves an arm64 box first, and an x86 engine image will not
start on it.

`ResourceRequirements.architecture` and `--arch` closed that for an operator
who knows their image's platforms (#390), and the deploy path now reads them
off the image's registry so an operator who does not is covered too (#405).
`archtrap_test.go` pins both halves against this catalog: it asserts that the
cheapest 80 GB shape is still the arm64 GH200, and that stating amd64 keeps
it out. **If a catalog refresh moves that, the test goes red on purpose** and
somebody should re-derive whether the trap still exists here rather than
updating the expectation.

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

**The stored name drops the comment's timestamp and adds a digest of the
key**, and both halves were paid for.

Dropping the timestamp came first: keeping it would give every
re-registration of the same key a new name, so one key would accumulate
entries. That made the name stable per operator and provider, and
re-registration a replace.

The digest came from the replace going wrong. **Lambda refuses to delete a key
any running instance references** ("Key is currently in use, cannot delete"),
so a second keystore claiming the shared name could not register, and
therefore could not rent at all, for as long as the first machine was up.
Two people on one account, or CI and a laptop, is enough. Hit live on the
second #427 rental, and it had been named as the open risk when the registrar
landed. With a digest in the name, nothing is ever deleted: an unchanged key
finds its own name, and a regenerated keypair is simply a second entry.

The cost is that an account can now hold several `iplane-` keys, so a prefix
scan cannot tell which private half the caller holds. The Provider remembers
the name it registered, and `Spawn` prefers that, falling back to the prefix
scan and then to the account's first key for an operator driving the adapter
by hand. The memo is reliable because `ensureProviderKey` runs immediately
before `Spawn` on every path that rents.

Lambda bounds the field at 1..64 characters, and the readable half is
truncated to make room rather than the digest being shortened, so two long
operator ids still get distinct names.

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

## What the first live rental measured

Driven 2026-08-24 on `gpu_1x_a10` in `us-east-1`, $1.29/hr. Full write-up in
[docs/design/0010](../../../docs/design/0010-lambda-validation-findings.md);
the numbers worth having to hand:

**Boot is slow and the address arrives well before the box does.** Launch
returned in 8.9s with status `booting` and no address. The public IP appeared
71 seconds later. The status did not reach `active` for another **2m53s**.
That gap is the whole reason `WaitForSSHReady` exists, and it is wide enough
that the two-stage wait matters rather than being a formality.

**Cold start, launch to engine serving: 4m30s**, covering boot, a 10.5 GB
vLLM image pull and engine start on a 0.5B model. Lambda's bandwidth is good;
the image pull was not the slow part.

**The SSH user is `ubuntu`, not root, and it is not in the `docker` group.**
That broke the shared executor on its first contact with the provider. The
lore is in [internal/deployments/sshdocker/NOTES.md](../../deployments/sshdocker/NOTES.md)
rather than here, because it is a property of stock cloud images rather than
of Lambda.

**`actions.terminate.available` is `false` for the first few seconds** after
launch and true from then on. Nothing reads it yet; worth knowing before
something does.

**The live record carries `is_reserved` and `workspace_id`, and the published
OpenAPI document declares neither.** `testdata/openapi-shapes.json` is
therefore a floor rather than a complete picture: the drift test checks that
everything the adapter decodes is declared, and the reverse does not hold.

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

## Filesystems, and why the handle is a name

`docs/design/0004` recorded that `POST /file-systems` returns 405 and
concluded Lambda has no API-creatable filesystems. Lambda spells the
collection two ways: `GET /api/v1/file-systems` is hyphenated and `POST
/api/v1/filesystems` is not, and only the second takes a write. The 405 was
fired at the read path, and the wrong conclusion held for four weeks because
a 405 is a confident-sounding answer (#432).

**The handle this adapter issues is the filesystem name, not the uuid.**
`VolumeRef.ID` is opaque to everything outside the adapter, and the name is
what the rest of Lambda's API accepts: `file_system_names` at launch takes
names, and the mount point is derived from the name. Only DELETE wants the
uuid, so `DeleteVolume` resolves name to uuid and nothing else has to. The
other way round would put a lookup on the far busier launch path.

**Size is not a thing here.** No size at create, and the guest reports 8.0E,
so `VolumeSpec.SizeGB` is dropped and `VolumeRef.SizeGB` stays zero. Echoing
the requested figure back would put a number in `iplane model ls` that no
measurement supports.

**A filesystem cannot be deleted while an instance has it, and "terminating"
counts.** A DELETE seconds after a terminate returns
`filesystems/filesystem-in-use`. `DeleteVolume` reports that as a wait rather
than a failure, because the caller's next move is to retry rather than to
investigate.

## What the mount actually is

Measured on hardware 2026-08-24, two rentals, $0.24. Details in
[docs/design/0010](../../../docs/design/0010-lambda-validation-findings.md).

Attached by naming it in the launch request, and the instance record echoes
`file_system_names` back, so the attachment is readable rather than merely
accepted. On the host:

```
drwxr-xr-x 2 ubuntu ubuntu 0 /lambda/nfs/iplane-cache-probe
TARGET                         SOURCE       FSTYPE   OPTIONS
/lambda/nfs/iplane-cache-probe 120642b1-... virtiofs rw,nosuid,nodev,relatime
```

**virtiofs, not NFS**, despite the path. Owned by `ubuntu` and writable
without sudo, which is why the sshdocker executor can bind it as that user.
A container mounting it with `docker run -v` reads host writes and writes
back, and the writes survive the instance: a second machine launched against
the same filesystem read both files the first one left.

**Ownership persists and is not uniform.** A container writing as root leaves
files the `ubuntu` SSH user cannot overwrite. That matters for whether
staging is re-runnable, and it is the first thing to check when a second pin
of the same model behaves oddly.

**The mount is two-stage, unlike either other provider.** RunPod attaches a
volume by id when it creates the pod, one call. Here the filesystem has to be
named at launch before the host directory exists, and only then can the
executor bind it into the container. That is why `Spec.volume_ids` exists and
why `Spawn` reads it: by deploy time it is minutes too late to ask.

## min_ram_gb is unenforced here

Lambda publishes no system-RAM figure in `skus.go`, so the shared resolver's
"a fact the provider does not publish cannot filter" rule leaves `min_ram_gb`
alone, and the rent path never consults `/instance-types` to check it. So the
requirement is accepted and not applied.

That is a real hole rather than a design choice, and it is worth knowing before
someone relies on the flag. `/instance-types` carries a real `memory_gib` per
shape, so the fix is available; it needs a network call on the rent path, which
is why it has not been taken yet.
