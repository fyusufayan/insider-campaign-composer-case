SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

BACKEND  := backend
FRONTEND := frontend

STATICCHECK_VERSION := v0.8.2
# Same golang-migrate version as backend/go.mod; the CLI applies the same files the binaries embed.
MIGRATE_VERSION     := v4.19.1
MIGRATE             := go run -tags pgx5 github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)

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
test-frontend: ## Frontend unit tests + strict typecheck
	cd $(FRONTEND) && npm test
	cd $(FRONTEND) && npm run typecheck

# Unit test packages must never depend on testcontainers (and therefore Docker).
# Integration tests live under backend/test/integration behind the "integration" build tag.
.PHONY: check-unit-isolation
check-unit-isolation:
	@cd $(BACKEND) && if go list -deps -test ./... | grep -q '^github.com/testcontainers/'; then \
		echo "error: a unit test package depends on testcontainers; move it behind the integration build tag" >&2; exit 1; \
	fi
	@cd $(BACKEND) && if go list -deps ./cmd/... | grep -q '/internal/llm/anthropictest$$'; then \
		echo "error: a production binary depends on the test-only anthropictest package" >&2; exit 1; \
	fi

.PHONY: test-integration
test-integration: ## Integration tests (requires Docker)
	@docker info >/dev/null 2>&1 || { echo "error: Docker is required for integration tests" >&2; exit 1; }
	cd $(BACKEND) && go vet -tags=integration ./...
	cd $(BACKEND) && go test -tags=integration -race -count=1 -timeout 10m ./test/integration/...

# ---- Docker Compose stack (orchestration lives in docker-compose.yml) ----

.PHONY: up
up: ## Build and start the full stack (http://localhost:8000); Ctrl+C stops it
	docker compose up --build

.PHONY: up-detached
up-detached: ## Build and start in the background, wait until healthy
	docker compose up --build -d --wait

.PHONY: down
down: ## Stop and remove containers; keeps the database volume
	docker compose down

.PHONY: logs
logs: ## Follow logs of all services
	docker compose logs -f

.PHONY: smoke
smoke: ## HTTP smoke test against the running stack (needs curl, python3)
	scripts/smoke.sh

# Destroys the local database volume (all campaigns and the seed). The next
# `make up` starts from an empty volume: migrate + seed run from scratch.
.PHONY: reset-volumes
reset-volumes: ## DESTRUCTIVE: remove containers and the database volume (CONFIRM=yes)
	@[ "$${CONFIRM:-}" = yes ] || { echo "error: this deletes all local campaigns and data; re-run with CONFIRM=yes" >&2; exit 1; }
	docker compose down -v

# ---- Database (DATABASE_URL from the environment; see .env.example) ----

.PHONY: require-database-url
require-database-url:
	@[ -n "$${DATABASE_URL:-}" ] || { echo "error: DATABASE_URL is required (see .env.example)" >&2; exit 1; }

.PHONY: migrate
migrate: require-database-url ## Apply all migrations to DATABASE_URL
	@cd $(BACKEND) && url="pgx5://$${DATABASE_URL#*://}" && $(MIGRATE) -path migrations -database "$$url" up

# Seed inputs: SEED_USERS (5000), SEED_RANDOM_SEED (42), SEED_REFERENCE_TIME
# (2026-11-01T00:00:00Z). The same inputs always reproduce the same dataset.
.PHONY: seed
seed: require-database-url ## Load the deterministic seed (does nothing if users exist)
	cd $(BACKEND) && go run ./cmd/seed

.PHONY: seed-reset
seed-reset: require-database-url ## Replace customer data and reseed (refused if any campaign or delivery exists)
	cd $(BACKEND) && go run ./cmd/seed -reset

# For demos long after the default reference time ("last N days" rules are
# evaluated against now()). Not reproducible across days; the run logs its inputs.
.PHONY: seed-today
seed-today: require-database-url ## Reset and reseed with today's UTC midnight (same refusal rules as seed-reset)
	cd $(BACKEND) && SEED_REFERENCE_TIME="$$(date -u +%Y-%m-%dT00:00:00Z)" go run ./cmd/seed -reset

.PHONY: lint
lint: ## gofmt check + go vet + staticcheck
	@cd $(BACKEND) && out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	cd $(BACKEND) && go vet ./... && go vet -tags=integration ./... && go vet -tags=live ./test/live/...
	cd $(BACKEND) && go run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) ./...

.PHONY: fmt
fmt: ## Format Go code
	cd $(BACKEND) && gofmt -w .
