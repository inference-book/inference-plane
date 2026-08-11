# Demo 09e -- the fabric A/B.
#
# Like 08a, run.sh owns its own `iplane serve`, so there is no separate
# `make serve` and no ../common/serve.mk here. Just `make demo`.
#
# `make demo` is the GPU-free self-test and costs nothing: two mock engines
# with an injected latency delta, which the comparator has to recover. Run it
# before `make demo-paid`, because a measurement rig you have not tested is
# how you end up publishing noise.

REPO_ROOT := $(shell cd ../../.. && pwd)

build: ## Build the iplane binary from the repo root
	@$(MAKE) -C $(REPO_ROOT) build

demo: build ## GPU-free: validate the harness against an injected delta (free)
	bash run.sh

demo-paid: build ## Real GPUs on two fabrics. Costs money. Read README.md on host choice first.
	@test -n "$(DEMO_MODEL)" || (echo "DEMO_MODEL is required; see README.md" && exit 1)
	@test -n "$(DEMO_A_SKU)" || (echo "DEMO_A_SKU is required; see README.md" && exit 1)
	@test -n "$(DEMO_B_SKU)" || (echo "DEMO_B_SKU is required; see README.md" && exit 1)
	DEMO_PAID=1 bash run.sh

test: ## Unit-test the comparator (no engines, no network)
	cd $(REPO_ROOT)/examples && go test ./09-multi-gpu/09e-fabric-ab/...

.PHONY: build demo demo-paid test
