.PHONY: build fmt lint test vuln migrate-up migrate-down compose-up compose-down

build:
	go build ./cmd/...

fmt:
	gofumpt -w cmd internal api
	goimports -w cmd internal api

lint:
	golangci-lint run ./...

test:
	go test ./...

vuln:
	govulncheck ./...

migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate down-one

compose-up:
	../deploy/up.sh

compose-down:
	../deploy/down.sh
