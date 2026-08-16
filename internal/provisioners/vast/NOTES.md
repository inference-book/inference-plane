# Vast adapter notes

Implementation lore for `internal/provisioners/vast`. The package doc covers
the wire format (endpoint families, the GET-not-POST search, the space-form
`gpu_name`); this file is the harder-won material: what the marketplace does
that a catalog-shaped provider does not, and what it cost to find out.

Vast is the one **marketplace** adapter. Offers come from independent hosts and
vary in price, bandwidth, reliability, disk, fabric, and whether the host is
quietly broken. Most of what follows falls out of that single fact.

## Choosing a host

- **On Vast, the cheapest offer is cheap for a reason.** Vast is a marketplace of independent hosts, so ordering the offer search by `dph_total` alone selects for bad hosts. A price-only pick stalled a 0.5B model download for 30 minutes. `findOffer` therefore pushes two marketplace-quality floors server-side alongside the SKU/VRAM/fabric filters: `inet_down >= 1000` (Mbps) and `reliability2 >= 0.98`, tunable via `IPLANE_VAST_MIN_INET_DOWN_MBPS` / `IPLANE_VAST_MIN_RELIABILITY`, `0` to disable. Measured live 2026-08-11 on RTX 3090: the price-only cheapest advertised 357 Mbps at reliability 0.9558, the cheapest clearing the floors advertised 1009 Mbps at 0.9878 for 12% more per hour. **The floors are deliberately not on `ResourceRequirements`.** They are Vast marketplace columns, not workload requirements, and no caller ever wants a slow flaky host, so there is nothing for an operator to express. There is also **no automatic fallback** when the floors match nothing: a silent downgrade to a slow host is the exact failure they exist to prevent, so the search fails and the error names the floors and the env vars. Ordering stays cheapest-first; the floors only bound who is eligible to be cheapest.
- **One Vast `gpu_name` can be two different cards.** "A100 SXM4" and "A100 PCIE" each cover a 40 GB and an 80 GB part. The catalog therefore carries variant rows (`A100_SXM4_40GB`, `A100_PCIE_40GB`) that share a `WireName` with the 80 GB row and add a `VRAMMaxGb` ceiling, so `findOffer` sends a `gpu_ram` **band** rather than only a floor. Without the ceiling a 40 GB request matches the 80 GB part (80 clears a 40 floor), which is a quietly dearer rental and, in an A/B where both arms must carry the same card, a confound. The floor alone (issue #243) stopped the reverse error and is still enforced: asking for `A100_SXM4` never returns a 40 GB card. Verified live 2026-08-11: the variants return 40960 MB at ~$0.27/hr and the 80 GB rows 81920 MB at ~$0.67-0.80/hr. `WireName` is load-bearing: sending our token `A100_SXM4_40GB` would filter on a `gpu_name` that does not exist and return an empty list, which reads exactly like "no capacity".

## When a host is broken

- **A dead Vast host used to bill the full timeout.** The provider says when a container will never run (`cur_state: stopped` plus a `status_msg` carrying docker's verbatim error), and the readiness loop ignored it, so a broken host cost the whole engine-ready wait. `internal/provisioners/vast/failure.go` now aborts on a terminal `status_msg`. Keyed on the **message, not `cur_state`**: `stopped` occurs benignly early in a container's life, while both observed failures carried unambiguous text. `terminalSignatures` holds only strings seen on real failed hosts, and `transientSignatures` is checked **first** so `Retrying` wins over an error word in the same message. That ordering is load-bearing: docker prints `Retrying` while still working and hosts do recover, so a false positive kills healthy deploys, and a slow deploy is the normal case (a 10 GB image on community capacity routinely takes minutes). A describe failure against Vast's control API is explicitly **not** terminal, since that API goes slow in bursts and was observed recovering mid-deploy. Exposed to the shared readiness path as `provisioners.FailureReporter` (#265). **RunPod does not have the same shape of signal** -- that was assumed and then disproven by measurement; see `internal/provisioners/runpod/NOTES.md`.

### The two real failures, and why neither was predictable

Both hosts below cleared every pre-rent quality filter. Neither failure is
visible in any marketplace attribute, which is why the floors above are
necessary and not sufficient.

- **Broken IPv6 path to the registry CDN.** `error pulling image configuration:
  download failed after attempts=6: read tcp [2409:...]->[2600:9000:...]:443:
  read: connection reset by peer`. Both endpoints IPv6; `2600:9000::/24` is the
  CloudFront range fronting Docker Hub. The host advertised 1412 Mbps and
  reliability 0.9878.
- **Broken NVIDIA Container Device Interface.** `OCI runtime create failed: ...
  failed to inject CDI devices: unresolvable CDI devices .../gpu=0: unknown`.
  That host could not start *any* GPU container.

**A failed host stays the cheapest offer.** Retrying immediately after the
first failure rented the identical `machine_id`, because nothing records that
it just failed (issue #214). Routing around it needed the bandwidth floor
raised above that host's advertised figure, and the replacement was *cheaper*.

**Pre-flight before renting a multi-GPU box.** A 1-GPU slice on the same
`machine_id` costs cents and has already caught a host that could never have
served. See `examples/09-multi-gpu/09e-fabric-ab/README.md`.

## Fabric

Vast is the only provider that **measures** intra-node fabric (`bw_nvlink` per
offer), so its catalog entry is a pre-filter and the per-offer reading decides.
A "PCIE" SKU stays searchable precisely because some of those hosts are
bridged: machine 6566 was an `A100 PCIE` reporting 300 GB/s.

`FABRIC_SCOPE_NONE` pushes a `bw_nvlink <= 0` ceiling, which excludes
*measured* fabric but **cannot prove absence**: Vast reports 0 both for "no
link" and "never measured", and roughly a quarter of SXM machines reported zero
on boards that are physically always NVLinked. A bridge-capable card reading
zero therefore resolves to `FABRIC_SOURCE_UNKNOWN`, not `NONE`. Settling it
needs an on-box reading (`internal/engineagent`'s interconnect sensor).

## Volumes

Machine-scoped, not datacenter-scoped like RunPod's (`POST /api/v0/volumes/`
returns `Invalid machine id`), so `iplane model pin` does not port over as-is.
Issue #254.

## Agent delivery

The cheapest of the three paths, and the last to get one. RunPod needs an
entrypoint wrapper because the image's own entrypoint is what runs; the SSH
path needs a sidecar container. Vast hands us the startup script outright, so
the agent is two blocks above the engine in a file the adapter already writes.
No wrapper, no sidecar.

The env the agent reads was already arriving -- `rentEngine` forwards
`dep.GetEnv()` and the deploy path stamps the identity there. Only the launch
was missing.

The fetch itself is shared with RunPod via `engineagent.AgentPrelude`; only the
final `exec` differs, because Vast's script carries the whole argv while
Docker's ENTRYPOINT/CMD split needs a trailing `"$@"`.

**Verified on a real box 2026-08-11:** the prelude reaches the machine intact
(`onstart contains engine-agent: True`, with the release URL quoted correctly).
Registration itself was not observed end to end -- the run was torn down while
still CONFIGURING. Note also that a released agent older than the interconnect
sensor will register without a link reading, so validating the LINKS column
needs a release containing it.

## Bid pricing is a real second tier

Every offer carries `min_bid` alongside `dph_total`. Renting at the bid price
means a higher bidder can take the machine, which is what makes it cheaper, and
the discount is not marginal: machine 143692 quoted **$0.83/hr on-demand and
$0.13/hr reclaimable** on 2026-08-15, the same physical host both times.

Vast is the only one of the three providers with this. RunPod's catalog does
not expose a distinguishable tier and Lambda has no such concept, so a
reclaimable search across all providers is in practice a Vast search.

`min_bid` is a floor rather than a price. We quote it, which is the cheapest a
rental could settle at and not necessarily what one does settle at.

## cpu_ram belongs in the query, not the catalog

Vast reports `cpu_ram` per offer, so a `min_ram_gb` floor is pushed
server-side in `findOffer` and the marketplace judges the actual host. The
catalog projection deliberately does **not** carry system RAM as a result:
once a real per-candidate number is reachable, filtering on a tier estimate can
only wrongly exclude.

Same shape as the disk floor, and the same lesson as #281. The conversion is
1000 rather than 1024, matching `gpu_ram`, because hosts report round decimal
figures and a binary conversion rejects machines that are fine.
