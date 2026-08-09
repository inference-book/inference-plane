# Demo 08a -- cold-start distance (warm-cache pinning).
#
# Unlike the other chapter-7+ demos, this one does NOT include
# ../common/serve.mk: run.sh starts and stops its own `iplane serve`
# because `iplane model pin` needs the state-dir lock that serve holds
# for its lifetime. Running `make serve` alongside would collide on
# :8080 and the state lock. Just `make demo`.

REPO_ROOT := $(shell cd ../../.. && pwd)

build: ## Build the iplane binary from the repo root
	@$(MAKE) -C $(REPO_ROOT) build

demo: build ## Run the cold-vs-warm A/B (real GPUs; destroys them on exit)
	bash run.sh

.PHONY: build demo
