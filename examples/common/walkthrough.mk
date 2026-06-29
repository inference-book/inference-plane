# examples/common/walkthrough.mk
#
# Shared Makefile fragment for the demokit walkthroughs under examples/.
# A per-example Makefile sets its binary name (and any extra flags) and
# then includes this fragment for the baseline targets:
#
#   DEMO_BIN := router-in-path
#   include ../common/walkthrough.mk
#
# Overridable variables (set them before the include):
#
#   DEMO_BIN      binary name for `make build`        (default: dir basename)
#   RUN_DOC       generated walkthrough doc           (default: RUN.md)
#   RECORD_TRACE  scratch trace path for `make readme`(default: /tmp/iplane-<dir>.json)
#   DEMO_FLAGS    extra flags appended to demo/note   (e.g. --provider $(PROVIDER))
#   RECORD_FLAGS  extra flags for the record pass     (e.g. --provider local)
#
# Example-specific targets (a `serve` that boots the backing service, an
# integration harness, etc.) stay in the per-example Makefile after the
# include -- that's where each demo shows off what makes it different.

DEMO_BIN     ?= $(notdir $(CURDIR))
RUN_DOC      ?= RUN.md
RECORD_TRACE ?= /tmp/iplane-$(notdir $(CURDIR)).json

demo: ## Run the demokit walkthrough (interactive, TUI)
	go run . --tui $(DEMO_FLAGS)

note: ## Run the walkthrough in notebook mode (Bubble Tea cells)
	go run . --note $(DEMO_FLAGS)

readme: ## Regenerate $(RUN_DOC) by recording a real run, then rendering markdown
	go run . --non-interactive $(RECORD_FLAGS) --record $(RECORD_TRACE)
	go run . --doc md --from $(RECORD_TRACE) > $(RUN_DOC)

readme-static: ## Regenerate $(RUN_DOC) from the demo definition only -- mermaid + structure, no live execution
	go run . --doc md > $(RUN_DOC)

build: ## Build the example binary
	go build -o $(DEMO_BIN) .

.PHONY: demo note readme readme-static build
.DEFAULT_GOAL := demo
