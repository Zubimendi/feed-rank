# ──────────────────────────────────────────────────────────────────────────────
# FeedRank Makefile
# ──────────────────────────────────────────────────────────────────────────────

DATABASE_URL ?= postgres://feedrank:feedrank@localhost:5432/feedrank?sslmode=disable
REDIS_URL    ?= localhost:6379

.PHONY: all up down logs migrate migrate-down build test load-test seed lint fmt help

## ── Docker ──────────────────────────────────────────────────────────────────

# Bring the full stack up (Postgres + Redis + api + worker)
up:
	docker compose up --build -d
	@echo "✅  Stack is up. API → http://localhost:8080"

# Tear everything down
down:
	docker compose down

# Tail logs from all services
logs:
	docker compose logs -f

# Postgres + Redis only (useful during local dev when running api/worker natively)
infra:
	docker compose up -d postgres redis
	@echo "✅  Postgres on :5432 | Redis on :6379"

## ── Migrations (golang-migrate) ─────────────────────────────────────────────

# Run all pending UP migrations
migrate:
	migrate -path ./migrations -database "$(DATABASE_URL)" up

# Roll back the last migration
migrate-down:
	migrate -path ./migrations -database "$(DATABASE_URL)" down 1

# Drop everything and re-apply from scratch (dangerous — dev only)
migrate-reset:
	migrate -path ./migrations -database "$(DATABASE_URL)" drop -f
	$(MAKE) migrate

## ── Build ────────────────────────────────────────────────────────────────────

# Build both binaries to ./bin/
build:
	go build -o bin/api    ./cmd/api
	go build -o bin/worker ./cmd/worker
	@echo "✅  Built: bin/api, bin/worker"

# Build in pure fan-out mode (for load-test comparison)
build-pure:
	FANOUT_MODE=pure go build -ldflags "-X main.fanoutMode=pure" -o bin/api-pure    ./cmd/api
	FANOUT_MODE=pure go build -ldflags "-X main.fanoutMode=pure" -o bin/worker-pure ./cmd/worker
	@echo "✅  Built: bin/api-pure, bin/worker-pure"

## ── Test ─────────────────────────────────────────────────────────────────────

test:
	go test -v -race ./...

## ── Load Test (k6) ───────────────────────────────────────────────────────────

# Seed the DB with realistic data, then run the k6 benchmark
seed:
	go run ./loadtest/seed/main.go

# Run the hybrid benchmark
load-test:
	k6 run --out json=loadtest/results/hybrid.json loadtest/fanout_benchmark.js

# Run the pure fan-out benchmark (requires api/worker running with FANOUT_MODE=pure)
load-test-pure:
	FANOUT_MODE=pure k6 run --out json=loadtest/results/pure.json loadtest/fanout_benchmark.js

## ── Lint / Format ────────────────────────────────────────────────────────────

lint:
	golangci-lint run ./...

fmt:
	gofmt -w .

## ── Help ─────────────────────────────────────────────────────────────────────

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
