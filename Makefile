# ZenSys Gateway
#
# Thin wrappers around `go` commands. Make is not installed on Windows by
# default, so README.md documents the equivalent raw commands as the primary
# path; this file exists for CI and for Linux/macOS deployments.
#
# The race detector requires cgo, which on Windows needs a C compiler. Set CC
# once with:
#   go env -w CC=C:/path/to/mingw64/bin/gcc.exe
# The test targets below enable cgo themselves, so ordinary builds stay
# pure-Go and produce a static binary suitable for the deployment image.

GO       ?= go
BINARY   := bin/gateway
CONFIG   ?= configs/gateway.dev.yaml

.PHONY: help build test test-int vet fmt tidy run clean compose-up compose-down

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Compile a static gateway binary
	CGO_ENABLED=0 $(GO) build -o $(BINARY) ./cmd/gateway

test: ## Run unit tests with the race detector
	CGO_ENABLED=1 $(GO) test ./... -race

test-int: ## Run integration tests against the development stack
	CGO_ENABLED=1 $(GO) test ./tests/integration/... -race -count=1

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format all Go source
	$(GO) fmt ./...

tidy: ## Tidy module dependencies
	$(GO) mod tidy

run: ## Run the gateway against CONFIG
	$(GO) run ./cmd/gateway -config $(CONFIG)

clean: ## Remove build artefacts
	rm -rf bin

compose-up: ## Start the local development stack
	docker compose -f deploy/docker-compose.dev.yml up -d

compose-down: ## Stop the local development stack
	docker compose -f deploy/docker-compose.dev.yml down
