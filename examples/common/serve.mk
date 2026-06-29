# examples/common/serve.mk
#
# Shared `make serve` target for chapter-7+ demos. Each demo lives in
# examples/<demo>/ and includes this fragment from its Makefile:
#
#   include ../common/serve.mk
#
# Behavior:
#
#   - Rebuilds the iplane binary from the repo root via `make build`
#     so the demo always runs against the current source tree.
#   - Picks the config file in this order:
#       1. ./config.yaml in the demo directory (per-demo override)
#       2. <repo>/deploy/config.yaml (global default)
#     A demo whose narrative needs default behavior (e.g. demo 04 /
#     Beat 1: scheduler off) ships no config.yaml and falls through to
#     the global file. Demos that need scheduler / queue / TTL tweaks
#     ship their own config.yaml so global defaults stay quiet.
#   - Sets IPLANE_BACKEND_URL to 127.0.0.1:8000 (host-side compose
#     port) so vllm-mode demos reach the engine if the gpu profile is
#     up; mock-mode demos ignore the value.
#
# Why per-demo serve instead of one global `make serve`:
# enabling the scheduler globally (router.queue.servicers > 0) would
# break demo 04's Beat 1 "direct-forward" narrative for any reader who
# ran 04 with a 05-tuned config still active. Each demo owning its
# config means switching demos is a single `make serve` invocation
# away, no manual config-editing in between.

# REPO_ROOT resolves to the inference-plane repo root regardless of
# where this fragment is included from. lastword of MAKEFILE_LIST is
# this file's path; ../.. relative to it lands at the repo root.
REPO_ROOT     := $(shell cd $(dir $(lastword $(MAKEFILE_LIST)))/../.. && pwd)
DEMO_CONFIG   := $(CURDIR)/config.yaml
GLOBAL_CONFIG := $(REPO_ROOT)/deploy/config.yaml

serve: ## Run iplane serve with this demo's config (or fall back to global)
	@$(MAKE) -C $(REPO_ROOT) build
	@if [ -f $(DEMO_CONFIG) ]; then \
		echo "==> using demo config: $(DEMO_CONFIG)"; \
		CFG=$(DEMO_CONFIG); \
	else \
		echo "==> no demo config; using global: $(GLOBAL_CONFIG)"; \
		CFG=$(GLOBAL_CONFIG); \
	fi; \
	cd $(REPO_ROOT) && IPLANE_BACKEND_URL=http://127.0.0.1:8000 \
	  ./bin/iplane serve --config $$CFG

.PHONY: serve
