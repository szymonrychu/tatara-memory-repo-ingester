SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

REGISTRY ?= harbor.szymonrichert.pl
IMAGE_NAME ?= containers/tatara-memory-repo-ingester
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE_REF := $(REGISTRY)/$(IMAGE_NAME):$(VERSION)

MODULE := github.com/szymonrychu/tatara-memory-repo-ingester

.PHONY: all lint test build image tidy fmt clean
all: lint test build

tidy:
	go mod tidy

fmt:
	gofmt -s -w .
	goimports -w -local $(MODULE) .

lint:
	golangci-lint run ./... || [ $$? -eq 5 ]

test:
	CGO_ENABLED=1 go test ./... -race -count=1

build:
	CGO_ENABLED=1 go build -trimpath \
		-ldflags "-s -w \
		  -X $(MODULE)/internal/version.Version=$(VERSION) \
		  -X $(MODULE)/internal/version.Commit=$(COMMIT) \
		  -X $(MODULE)/internal/version.Date=$(DATE)" \
		-o bin/tatara-ingest ./cmd/tatara-ingest

image:
	docker buildx build --platform=linux/amd64 \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
		-t $(IMAGE_REF) --load .

clean:
	rm -rf bin
