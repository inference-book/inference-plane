demo: ## Run the demokit walkthrough (TUI) as a client of the running server
	go run . --tui

serve: ## Start the iplane provisioner service standalone on :9091 (RunPod debug logging on)
	IPLANE_RUNPOD_DEBUG=1 go run . --serve

readme: ## Regenerate RUN.md by recording a real demo run (requires make serve in another terminal)
	go run . --non-interactive --record /tmp/iplane-run.json
	go run . --doc md --from /tmp/iplane-run.json > RUN.md

readme-static: ## Regenerate RUN.md from the demo definition only -- mermaid diagram + structure, no live execution
	go run . --doc md > RUN.md

build: ## Build the binary
	go build -o end-to-end .

.PHONY: demo serve readme readme-static build
.DEFAULT_GOAL := demo
