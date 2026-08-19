# internal/router — NOTES

Lore for the data path. Read this before trusting a field on a Deployment
or assuming a demo's default.

## Scheduler defaults to OFF

`router.queue.servicers` defaults to `0` in `deploy/config.yaml`, which means no scheduler is constructed and the router takes the direct-forward path (Beat 1 behavior). The v0.2 release/v0.2 snapshot ships with this default to avoid surprising operators on existing deploys; demo 05 requires `servicers > 0` (and `in_flight_cap > 0` for the queue-pressure story to be visible). Documented in `examples/05-fair-queueing/README.md`'s troubleshooting section.

## Priority is request-level, not deployment-level

When tempted to put routing/queueing policy on a runtime artifact (Deployment, Instance), check whether the property describes the artifact itself or the *traffic flowing through it*. If traffic, it belongs at the routing layer, not on the artifact. See `protos/provisioner/v1/types.proto`'s reserved-field-22 comment for the receipt; PR 131 corrected this mid-review.

## `engine_endpoint` (singular) is legacy; `engine_endpoints` (plural) is the real per-replica set

The plural, parallel to `instance_ids`, is what the router routes over (`effectiveEndpoints`). The singular is only stamped for the `count==1` case (`recordCreateSlots`) and by slot-0's deploy emit. A multi-replica deployment created *directly* (not scaled up from 1) never stamps the singular — so any readiness/eligibility check must be plural-aware (`hasStampedEndpoint`), not singular-only. Demo 06 dodged this by starting at 1 replica then scaling; the external provider (first from-scratch multi-replica path) exposed it.

## Prefix-affinity (Ch 8) essentials

Toggle via `router.routing_policy: round_robin | prefix_affinity` on `iplane serve` (config/env, a startup property — not per-request). `router.affinity_overload_threshold` (0 = off) enables the load-aware override: when a pinned replica has >= N in-flight, spill that turn to the coolest replica but *keep the pin* (temporary detour; the session snaps back when it cools). Affinity keys on `X-IPlane-Session`; header-less clients (a plain OpenAI SDK) get a body-derived key — `hash(first system msg + first user msg)` — but ONLY on the flat `/v1/chat/completions` URL, which already parses the body for `model`. The deploy-id URL streams the body unparsed and stays header-only. The `iplane.router.affinity.total` hit-rate is a router-side routing-locality **proxy**, not the engine's `gpu_prefix_cache_hit_rate` (real engine scraping is deferred to issue 51); don't conflate them. `iplane mock-engine --latency 3ms` keeps routing demos fast — see `examples/07-prefix-affinity/`.

