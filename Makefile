.PHONY: help up down logs build test vet fmt smoke reset import tidy

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

up: ## Start the stack (postgres, redis, api)
	docker compose up -d --build

down: ## Stop the stack
	docker compose down

reset: ## Stop and wipe the database volume
	docker compose down -v

logs: ## Follow API logs
	docker compose logs -f api

build: ## Compile the binary
	go build -o bin/api ./cmd/api

test: ## Run unit tests
	go test ./... -count=1

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go source
	gofmt -w .

tidy: ## Tidy module dependencies
	go mod tidy

smoke: ## Run end-to-end checks against a running stack
	bash scripts/smoke.sh

import: ## Import GeoNames data: make import COUNTRY=TR
	go run ./cmd/api -import ./data -country $(COUNTRY)
