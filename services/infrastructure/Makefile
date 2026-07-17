BINARY_NAME := lettuce-server
BUILD_DIR := bin
GO := go
GOFLAGS := -trimpath

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)

.PHONY: all build test lint proto migrate-up migrate-down clean

all: lint test build

build:
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/lettuce-server

test:
	$(GO) test ./...

lint:
	golangci-lint run

proto:
	protoc --go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		proto/lettuce/v1/*.proto

migrate-up:
	$(GO) run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1 \
		-source "file://migrations" -database "$(LETTUCE_DATABASE_URL)" up

migrate-down:
	$(GO) run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1 \
		-source "file://migrations" -database "$(LETTUCE_DATABASE_URL)" down

clean:
	rm -rf $(BUILD_DIR)
