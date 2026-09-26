.PHONY: generate build test run check-generated

generate:
	go tool sqlc generate

build:
	go build -o bin/photos ./cmd/photos

test:
	go vet ./...
	go test -race ./...

# Fails if generated sqlc code is out of date (use in CI).
check-generated:
	go tool sqlc diff

# Run locally with variables from .env (which is git-ignored).
run:
	set -a && . ./.env && set +a && go run ./cmd/photos
