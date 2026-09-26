.PHONY: generate build test run

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
