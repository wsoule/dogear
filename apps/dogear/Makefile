.PHONY: dev test sqlc migrate-up import linkcheck vet build

# Load .env when present.
ifneq (,$(wildcard .env))
include .env
export
endif

dev: migrate-up
	go run ./cmd/dogear serve

test:
	go test ./...

vet:
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

sqlc:
	cd internal/db && sqlc generate

migrate-up:
	go run ./cmd/dogear migrate

import:
	go run ./cmd/dogear import

linkcheck:
	go run ./cmd/dogear linkcheck

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/dogear ./cmd/dogear
