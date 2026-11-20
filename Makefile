# SPDX-License-Identifier: AGPL-3.0-only

PKG := plainroute
VERSION := dev
BIN := bin

BENCH_URL ?= http://127.0.0.1:8080
BENCH_MODEL ?= bench

.PHONY: all build test race vet fmt lint headers run bench e2e

all: build test

build:
	mkdir -p $(BIN)
	go build -ldflags "-X main.version=$(VERSION)" -o $(BIN)/plainroute ./cmd/plainroute
	go build -o $(BIN)/plainroute-fakeupstream ./cmd/fakeupstream
	go build -o $(BIN)/plainroute-bench ./cmd/bench

test:
	go test ./...

race:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

fmt:
	test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

lint:
	golangci-lint run

# The licence identifier must be in the header of every file, apart from the licence text
# itself and go.sum, which cannot carry a comment. CI runs the same script.
headers:
	./scripts/check-headers.sh

run:
	go run ./cmd/plainroute -config config.example.yaml

# bench sends load to a gateway that is already running and then reads the gateway's
# own counters. The two sets of numbers must agree.
bench:
	go run ./cmd/bench -url $(BENCH_URL) -model $(BENCH_MODEL) -requests 1000 -concurrency 32

e2e:
	./scripts/e2e.sh
