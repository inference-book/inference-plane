PROVIDER ?= local

demo: ## Run the demokit walkthrough driving the iplane CLI (TUI)
	go run . --tui --provider $(PROVIDER)

readme: ## Regenerate RUN.md by recording a real demo run against local
	go run . --non-interactive --provider local --record /tmp/iplane-cli-run.json
	go run . --doc md --from /tmp/iplane-cli-run.json > RUN.md

readme-static: ## Regenerate RUN.md from the demo definition only -- mermaid + structure, no live execution
	go run . --doc md > RUN.md

build: ## Build the example binary
	go build -o cli-end-to-end .

.PHONY: demo readme readme-static build
.DEFAULT_GOAL := demo
