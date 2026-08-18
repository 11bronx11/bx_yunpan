.PHONY: build fmt lint test vuln proto proto-check migrate-up migrate-down compose-up compose-down

PROTO_FILES := $(shell find api/proto -name '*.proto')

build:
	go build ./cmd/...

# 生成物提交进仓库，CI 不安装 protoc。
# module= 模式按 go_package 剥离模块前缀，生成物平铺到 internal/ai/pb。
proto:
	protoc --proto_path=api/proto \
		--go_out=module=github.com/11bronx11/bx_yunpan/backend:. \
		--go-grpc_out=module=github.com/11bronx11/bx_yunpan/backend:. \
		$(patsubst api/proto/%,%,$(PROTO_FILES))

# 防 proto 改了忘生成。
proto-check: proto
	git diff --exit-code -- internal/ai/pb

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
	../deploy/local-run.sh migrate up

migrate-down:
	../deploy/local-run.sh migrate down-one

compose-up:
	../deploy/up.sh

compose-down:
	../deploy/down.sh
