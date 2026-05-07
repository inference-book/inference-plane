.PHONY: build up down pull smoke load logs dashboards clean check-pins help

# Default target: list available targets.
help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ── Local code ──────────────────────────────────────────────────────────
build: ## Compile the controlplane binary into bin/
	@mkdir -p bin
	go build -o bin/controlplane ./cmd/controlplane

# ── Stack lifecycle ─────────────────────────────────────────────────────
up: ## Bring up vLLM, control plane, OTel collector, Grafana stack
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml up -d

down: ## Tear the stack down
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml down

pull: ## Pre-pull all container images (warms the cache before `up`)
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml pull

# ── Verification ────────────────────────────────────────────────────────
smoke: ## Run smoke tests against a live stack (assumes `make up` has run)
	go test -tags=smoke -v -count=1 ./tests/smoke/...

load: ## Run a small load test (costs real GPU time on rented hardware!)
	@echo "load: not implemented yet (planned: k6 or vegeta-based test)"
	@exit 1

test: ## Run unit tests (no live stack needed)
	go test ./...

# ── Inspection ──────────────────────────────────────────────────────────
logs: ## Tail logs from all services
	docker compose -f deploy/docker-compose.yaml logs -f --tail=100

dashboards: ## Open the Grafana UI in the default browser
	@echo "Grafana: http://localhost:3000  (admin / admin)"
	@command -v open >/dev/null && open http://localhost:3000 || true

# ── Pinning ─────────────────────────────────────────────────────────────
check-pins: ## Verify pinned-versions.env matches the book's pinned-versions.tex
	@PIN_TEX=../book/src/styles/pinned-versions.tex \
	 PIN_ENV=pinned-versions.env \
	 sh ../book/scripts/check-pins.sh

# ── Cleanup ─────────────────────────────────────────────────────────────
clean: ## Remove build artifacts and local data volumes
	rm -rf bin dist tmp deploy/data
