SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BACKEND  := backend
FRONTEND := frontend

STATICCHECK_VERSION := v0.8.2

.DEFAULT_GOAL := help

.PHONY: help
help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

.PHONY: test
test: test-backend test-frontend ## Unit tests (no Docker, no API key)

.PHONY: test-backend
test-backend: check-unit-isolation ## Go vet + unit tests
	cd $(BACKEND) && go vet ./...
	cd $(BACKEND) && go test ./...

.PHONY: test-frontend
test-frontend: ## Frontend unit tests (skipped until the frontend exists)
	@if [ -f $(FRONTEND)/package.json ]; then cd $(FRONTEND) && npm test; \
	else echo "frontend: not present yet, skipping"; fi

# Unit test packages must never depend on testcontainers (and therefore Docker).
# Integration tests live under backend/test/integration behind the "integration" build tag.
.PHONY: check-unit-isolation
check-unit-isolation:
	@cd $(BACKEND) && if go list -deps -test ./... | grep -q '^github.com/testcontainers/'; then \
		echo "error: a unit test package depends on testcontainers; move it behind the integration build tag" >&2; exit 1; \
	fi

.PHONY: test-integration
test-integration: ## Integration tests (requires Docker)
	@docker info >/dev/null 2>&1 || { echo "error: Docker is required for integration tests" >&2; exit 1; }
	cd $(BACKEND) && go vet -tags=integration ./...
	cd $(BACKEND) && go test -tags=integration -race -count=1 -timeout 10m ./test/integration/...

.PHONY: lint
lint: ## gofmt check + go vet + staticcheck
	@cd $(BACKEND) && out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	cd $(BACKEND) && go vet ./... && go vet -tags=integration ./...
	cd $(BACKEND) && go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

.PHONY: fmt
fmt: ## Format Go code
	cd $(BACKEND) && gofmt -w .
