.PHONY: build up down infra-up infra-down rebuild pull smoke load logs dashboards clean check-pins help install examples dist dist-checksums dist-clean

PKG    := ./cmd/iplane

# Default target: list available targets.
help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

install:
	go install $(PKG)
	@echo "installed to $$(go env GOBIN || echo $$(go env GOPATH)/bin)/$(BINARY)"

# ── Local code ──────────────────────────────────────────────────────────
# Stamped, because `iplane load --sweep` records the build that produced
# a measurement and an unstamped binary writes "dev" into a data artifact
# a book figure is drawn from (#347). No -s -w here, unlike dist: a local
# binary is one somebody may want to attach a debugger to.
build: ## Compile the iplane binary into bin/ (version stamped)
	@mkdir -p bin
	go build -ldflags "$(VERSION_LDFLAGS)" -o bin/iplane ./cmd/iplane

# ── Release artifacts ───────────────────────────────────────────────────
# VERSION is stamped into the binary at link time. It defaults to the
# git describe of the working tree so a local `make dist` produces
# something identifiable, and the release workflow overrides it with the
# tag being built.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS := -X github.com/inference-book/inference-plane/internal/version.Version=$(VERSION)
LDFLAGS    := -s -w $(VERSION_LDFLAGS)
DIST_PLATFORMS := linux/amd64 linux/arm64

# Linux binaries exist for one reason: the engine agent runs inside an
# engine container on a rented box, and it gets there by being fetched
# from a published URL. A macOS build cannot be copied onto a linux pod,
# so cross-compilation is the delivery path's prerequisite rather than a
# convenience. CGO is off so the result is static and runs on whatever
# libc the engine image happens to ship.
dist: ## Cross-compile release binaries into dist/
	@mkdir -p dist
	@for p in $(DIST_PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "  building dist/iplane-$$os-$$arch ($(VERSION))"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/iplane-$$os-$$arch $(PKG) || exit 1; \
	done

dist-checksums: dist ## Cross-compile and write dist/checksums.txt
	@cd dist && shasum -a 256 iplane-* > checksums.txt
	@echo "  wrote dist/checksums.txt"

dist-clean: ## Remove dist/
	rm -rf dist

# ── Stack lifecycle ─────────────────────────────────────────────────────
# Two paths share one compose file via profiles:
#   * `infra-up`/`infra-down` (default profile): obs services only.
#     Pair with `make serve` from a specific demo dir (e.g.
#     `cd examples/05-fair-queueing && make serve`) so the daemon picks
#     up the demo's config.yaml. Demos that don't ship a config.yaml
#     fall back to the global deploy/config.yaml.
#   * `up`/`down` (--profile fullstack): everything in Docker, including
#     the controlplane container built from the local Dockerfile.
#     This is the reader's one-command path.
infra-up: ## Bring up infra only (obs services); host iplane via `cd examples/<demo> && make serve`
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml up -d

infra-down: ## Tear down infra services
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml down

up: ## Bring up the full stack incl. controlplane container (the readers' path)
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml --profile fullstack up -d --build

down: ## Tear the full stack down
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml --profile fullstack down

pull: ## Pre-pull external images (skips the locally-built controlplane)
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml --profile fullstack pull --ignore-buildable

rebuild: ## Rebuild local Docker images without starting the stack (currently: controlplane)
	docker compose --env-file pinned-versions.env -f deploy/docker-compose.yaml --profile fullstack build

# ── Verification ────────────────────────────────────────────────────────
smoke: ## Run smoke tests against a live stack (assumes `make up` has run)
	go test -tags=smoke -v -count=1 ./tests/smoke/...

smoke-runpod: ## Provision a real RunPod pod ($0.05 ish) -- requires RUNPOD_API_KEY
	@test -n "$$RUNPOD_API_KEY" || (echo "RUNPOD_API_KEY not set" && exit 1)
	go test -tags=smoke_runpod -v -count=1 -timeout=5m ./tests/smoke-runpod/...

smoke-vast: ## Hit the real Vast.ai API (List is free; VAST_RENT=1 also rents + terminates an RTX 3090) -- requires VAST_API_KEY
	@test -n "$$VAST_API_KEY" || (echo "VAST_API_KEY not set" && exit 1)
	go test -tags=smoke_vast -v -count=1 -timeout=5m ./tests/smoke-vast/...

.PHONY: smoke-vast-offers
smoke-vast-offers: ## Verify Vast still honours the marketplace-quality floors (search only, never rents) -- requires VAST_API_KEY
	@test -n "$$VAST_API_KEY" || (echo "VAST_API_KEY not set" && exit 1)
	go test -tags=smoke_vast -v -count=1 -timeout=2m ./internal/provisioners/vast/...

smoke-lambdalabs: ## Hit the real Lambda Labs API (List is free; LAMBDA_RENT=1 also rents + terminates an A10) -- requires LAMBDA_API_KEY
	@test -n "$$LAMBDA_API_KEY" || (echo "LAMBDA_API_KEY not set" && exit 1)
	go test -tags=smoke_lambdalabs -v -count=1 -timeout=5m ./tests/smoke-lambdalabs/...

# The load generator refuses to guess a model, because the string has to
# match a live deployment exactly and a wrong one sends traffic nowhere.
# mock/mock is what the mock-engine demos deploy under, so the bare
# target works against a mock stack. Point it at a real deployment with
# `make load MODEL=...`, using the string `iplane deployment list` prints.
MODEL ?= mock/mock

load: ## Generate synthetic traffic against the running stack (MODEL=... for a real deployment, defaults to the mock)
	go run ./cmd/iplane load --url=http://localhost:8080 --model=$(MODEL)

test: ## Run unit tests (no live stack needed)
	go test ./...

examples: ## Build the demokit walkthroughs (separate module under examples/; keeps demokit out of the control-plane deps)
	cd examples && go build ./...

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
