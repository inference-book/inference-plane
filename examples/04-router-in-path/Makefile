demo: ## Run the demokit walkthrough (TUI). Requires `iplane serve` running separately.
	go run . --tui

readme: ## Regenerate RUN.md by recording a real run against `iplane serve` + RunPod
	go run . --non-interactive --record /tmp/iplane-router-run.json
	go run . --doc md --from /tmp/iplane-router-run.json > RUN.md

readme-static: ## Regenerate RUN.md from the demo definition only -- structure + mermaid, no live execution
	go run . --doc md > RUN.md

build: ## Build the example binary
	go build -o router-in-path .

.PHONY: demo readme readme-static build
.DEFAULT_GOAL := demo
