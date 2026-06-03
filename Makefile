.PHONY: build up down pull smoke load logs dashboards clean check-pins help install

PKG    := ./cmd/iplane

# Default target: list available targets.
help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

install:
	go install $(PKG)
	@echo "installed to $$(go env GOBIN || echo $$(go env GOPATH)/bin)/$(BINARY)"

# ── Local code ──────────────────────────────────────────────────────────
build: ## Compile the iplane binary into bin/
	@mkdir -p bin
	go build -o bin/iplane ./cmd/iplane

# ── Stack lifecycle ─────────────────────────────────────────────────────
up: ## Bring up the stack (builds the controlplane image locally)
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml up -d --build

down: ## Tear the stack down
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml down

pull: ## Pre-pull external images (skips the locally-built controlplane)
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml pull --ignore-buildable

build-image: ## Build the controlplane Docker image without starting the stack
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml build controlplane

# ── Verification ────────────────────────────────────────────────────────
smoke: ## Run smoke tests against a live stack (assumes `make up` has run)
	go test -tags=smoke -v -count=1 ./tests/smoke/...

smoke-runpod: ## Provision a real RunPod pod ($0.05 ish) -- requires RUNPOD_API_KEY
	@test -n "$$RUNPOD_API_KEY" || (echo "RUNPOD_API_KEY not set" && exit 1)
	go test -tags=smoke_runpod -v -count=1 -timeout=5m ./tests/smoke-runpod/...

smoke-vast: ## Hit the real Vast.ai API (List is free; VAST_RENT=1 also rents + terminates an RTX 3090) -- requires VAST_API_KEY
	@test -n "$$VAST_API_KEY" || (echo "VAST_API_KEY not set" && exit 1)
	go test -tags=smoke_vast -v -count=1 -timeout=5m ./tests/smoke-vast/...

load: ## Generate synthetic traffic against the running stack (safe with mock backend)
	go run ./cmd/iplane load --url=http://localhost:8080

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

gen-names: ## Regenerate names.go and metric-names.tex from metric-names.yaml
	go run ./cmd/iplane gen-names

check-names: ## Verify generated name files are up-to-date with the YAML schema (CI runs this)
	@go run ./cmd/iplane gen-names
	@if ! git diff --quiet -- internal/telemetry/names.go ../book/src/styles/metric-names.tex; then \
		echo "ERROR: generated name files are out of sync with metric-names.yaml."; \
		echo "Run 'make gen-names' and commit the result."; \
		git diff --stat -- internal/telemetry/names.go ../book/src/styles/metric-names.tex; \
		exit 1; \
	fi
	@echo "name files in sync with metric-names.yaml"

# ── Architectural constraints ───────────────────────────────────────────
check-constraints: ## Verify CONSTRAINTS.md architectural rules (CI runs this)
	@matches=$$(grep -rln '"github.com/inference-book/inference-plane/internal/provisioners"' internal/router internal/dataplane 2>/dev/null || true); \
	if [ -n "$$matches" ]; then \
		echo "ERROR: CP/DP-1 violation -- data-plane code imports internal/provisioners directly."; \
		echo "$$matches"; \
		echo "See CONSTRAINTS.md for the rationale and the gRPC-client-only pattern."; \
		exit 1; \
	fi
	@echo "CONSTRAINTS.md: CP/DP-1 satisfied"

# ── Cleanup ─────────────────────────────────────────────────────────────
clean: ## Remove build artifacts and local data volumes
	rm -rf bin dist tmp deploy/data
