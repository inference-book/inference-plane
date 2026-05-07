# inference-plane

Reference implementation of the Control Plane built throughout
*Inference Is All You Need: A Systems Engineer's Guide* (Apress, 2026).

The repository evolves alongside the book. Each Part of the book
corresponds to a versioned release of the control plane:

| Version | Chapter | Adds                                             |
| ------- | ------- | ------------------------------------------------ |
| v0.1    | 6       | Backend abstraction, OTel observability, health  |
| v0.2    | 7–10    | Auth, rate limiting, caching, queueing           |
| v0.3    | 12–15   | Multi-tenancy, billing, multi-backend routing    |
| v1.0    | 18–19   | Production hardening, autoscaling, full platform |

Each version is published as a maintained branch (`release/vX.Y`)
that carries fixes cherry-picked from `main`, plus an immutable tag
(`vX.Y.0`) marking the chapter's text-match snapshot. Check out the
branch by default; pin to the tag if you need byte-identical match
to the book.

## v0.1 (this branch)

Single-backend control plane proxying to **vLLM** serving
**Qwen 3.5 8B**. Observability via the OpenTelemetry Collector
exporting to a local Grafana stack (Tempo, Loki, Mimir).

## Quick start

```sh
git checkout release/v0.1
make up        # bring up the stack (vLLM + control plane + observability)
make smoke     # verify everything is healthy
```

See `Makefile` for the full target list.

## Repository layout

```
cmd/controlplane/      main.go entrypoint
internal/
  backend/             Backend interface + engine implementations
  server/              HTTP handlers, middleware, routing
  telemetry/           OTel SDK setup (tracer, meter, logger providers)
  config/              YAML + env config loading
deploy/
  docker-compose.yaml  vLLM, control plane, OTel collector, Grafana stack
  grafana/             provisioned dashboards and datasources
  *-config.yaml        per-component configuration
scripts/
  smoke.sh             health check
  load.sh              k6/oha load test
pinned-versions.env    model, engine, stack version pins (synced with book)
providers.yaml         multi-provider rate table
```

## License

Apache 2.0. See `LICENSE`.
