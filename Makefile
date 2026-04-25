# Reproducible build for runlog-verifier.
# Outputs are deterministic given the same toolchain + source — required
# for the trust model in docs/03-verification-and-provenance.md §5.4.

VERSION ?= dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -trimpath -buildvcs=false -ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -s -w"

.PHONY: build test fmt vet tidy clean

build:
	go build $(LDFLAGS) -o bin/runlog-verifier ./cmd/runlog-verifier

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/
