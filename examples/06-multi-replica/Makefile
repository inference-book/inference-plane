# Demo 06 is a shell-script walkthrough -- no Go demokit binary --
# so walkthrough.mk doesn't apply. Just the serve fragment.
include ../common/serve.mk

demo: ## Run the multi-replica throughput-curve walkthrough
	@if [ -z "$(DEPLOY_ID)" ]; then \
		echo "usage: make demo DEPLOY_ID=<deployment-id>" >&2; \
		echo "  hint: run examples/04-router-in-path first to provision a deployment" >&2; \
		exit 2; \
	fi
	bash run.sh $(DEPLOY_ID)

.PHONY: demo
