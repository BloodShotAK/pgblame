TARGET_DSN ?= postgres://pgblame:pgblame@localhost:54321/app?sslmode=disable
STORE_DSN  ?= postgres://postgres:postgres@localhost:54322/pgblame?sslmode=disable
# Integration tests reset pg_stat_statements, which needs a superuser.
TEST_DSN   ?= postgres://postgres:postgres@localhost:54321/app?sslmode=disable

.PHONY: build test test-integration vet up down logs check top regressions harness overhead

build:
	go build -o bin/pgblame ./cmd/pgblame

test:
	go test ./...

test-integration:
	PGBLAME_TEST_DSN='$(TEST_DSN)' go test -count=1 -run Integration ./...

vet:
	go vet ./...

up:
	docker compose up -d --build

down:
	docker compose down -v

logs:
	docker compose logs -f collector

check:
	go run ./cmd/pgblame check --target-dsn '$(TARGET_DSN)'

top:
	go run ./cmd/pgblame top --store-dsn '$(STORE_DSN)'

regressions:
	go run ./cmd/pgblame regressions --store-dsn '$(STORE_DSN)' --target local --window 2m

# Both create and drop their own database on the target: local stack only.
harness:
	docker compose exec -T store createdb -U postgres pgblame_harness 2>/dev/null || true
	go run ./cmd/harness -yes -target-dsn '$(TEST_DSN)' \
		-store-dsn 'postgres://postgres:postgres@localhost:54322/pgblame_harness?sslmode=disable'

overhead:
	go run ./cmd/overhead -yes -target-dsn '$(TEST_DSN)'
