DEMO_BIN := end-to-end
include ../common/walkthrough.mk

# ── Example-specific ──────────────────────────────────────────────
# 01 ships a standalone provisioner service so the walkthrough can run
# as one client of a real connect-rpc server (point your own tooling at
# :9091 to exercise the same surface independently).

serve: ## Start the iplane provisioner service standalone on :9091 (RunPod debug logging on)
	IPLANE_RUNPOD_DEBUG=1 go run . --serve

.PHONY: serve
