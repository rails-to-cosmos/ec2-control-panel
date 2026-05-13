BINARY := ec2cp
PKG    := ./cmd/ec2cp

.DEFAULT_GOAL := build

.PHONY: build run serve test fmt vet tidy clean dev docker-up docker-down help

build: ## Build the static binary
	go build -ldflags="-s -w" -o $(BINARY) $(PKG)

run: ## Run the CLI (use ARGS="<subcommand> <args>")
	go run $(PKG) $(ARGS)

serve: ## Run the HTTP server on port 2721
	go run $(PKG) serve --port 2721

test: ## Run all tests
	go test ./...

fmt: ## Format Go source with gofmt
	gofmt -s -w .

vet: ## Run go vet across all packages
	go vet ./...

tidy: ## Sync go.mod / go.sum
	go mod tidy

clean: ## Remove the local binary
	rm -f $(BINARY)

dev: ## Install dev tools (gopls, etc.)
	go install golang.org/x/tools/gopls@latest

docker-up: ## Rebuild and start the docker-compose stack
	./run.sh

docker-down: ## Stop the docker-compose stack
	docker compose down

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?##' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "} {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
