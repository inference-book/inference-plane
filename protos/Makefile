.PHONY: gen lint deps update help

help:
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

deps: ## Resolve module dependencies into buf.lock
	buf dep update

gen: ## Generate Go code into ../gen/go (committed)
	buf generate

lint: ## Lint proto files
	buf lint

update: ## Refresh deps then regenerate
	buf dep update
	buf generate
