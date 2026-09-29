.PHONY: help build run worker migrate platform-key test test-integration vet fmt check tidy db-up db-down db-reset smoke docker-build clean

BINARY := bin/billing
VERSION ?= dev
TEST_DSN := postgres://postgres:postgres@localhost:5434/billing_test?sslmode=disable

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

build: ## Compile the binary
	@mkdir -p bin
	go build -ldflags="-X main.version=$(VERSION)" -o $(BINARY) ./cmd/billing

run: ## Run the API + UI (+ worker) with .env loaded
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/billing serve

worker: ## Run only the billing engine and webhook dispatcher
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/billing worker

migrate: ## Apply migrations, then exit
	@set -a; [ -f .env ] && . ./.env; set +a; go run ./cmd/billing migrate

platform-key: ## Mint a platform admin key (manages tenants)
	@set -a; [ -f .env ] && . ./.env; set +a; \
		go run ./cmd/billing keys create --name platform-admin --scopes admin --platform

test: ## Unit tests (database tests skip)
	go test ./... -race -count=1

test-integration: db-up ## Unit + database tests
	@docker compose exec -T postgres psql -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname='billing_test'" | grep -q 1 || \
		docker compose exec -T postgres psql -U postgres -q -c "CREATE DATABASE billing_test"
	TEST_DATABASE_URL=$(TEST_DSN) go test ./... -race -count=1

vet: ## go vet
	go vet ./...

fmt: ## gofmt
	gofmt -w .

tidy: ## go mod tidy
	go mod tidy

check: fmt vet test-integration ## Everything to run before a commit

smoke: ## End-to-end curl walkthrough against a running server (see scripts/smoke.sh)
	./scripts/smoke.sh

db-up: ## Start Postgres on :5434
	docker compose up -d postgres
	@until docker compose exec -T postgres pg_isready -U postgres -d billing >/dev/null 2>&1; do sleep 1; done
	@echo "postgres ready on localhost:5434"

db-down: ## Stop Postgres
	docker compose down

db-reset: ## Drop and recreate the local database
	docker compose down -v
	$(MAKE) db-up

docker-build: ## Build the image
	docker build -t billing:$(VERSION) --build-arg VERSION=$(VERSION) .

clean: ## Remove build output
	rm -rf bin
