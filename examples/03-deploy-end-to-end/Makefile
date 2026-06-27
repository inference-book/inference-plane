DEMO_BIN := deploy-end-to-end
include ../common/walkthrough.mk

# ── Example-specific ──────────────────────────────────────────────
# 03 carries the full chapter-6 flow (provision → deploy → chat →
# destroy) and runs as a client of the provisioner+deployment service.

serve: ## Start the iplane provisioner+deployment service on :9091 (RunPod debug logging on)
	IPLANE_RUNPOD_DEBUG=1 go run . --serve

.PHONY: serve
