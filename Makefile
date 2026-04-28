# Reproducible build for runlog-verifier.
# Outputs are deterministic given the same toolchain + source — required
# for the trust model in docs/03-verification-and-provenance.md §5.4.

VERSION ?= dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -trimpath -buildvcs=false -ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -s -w"

.PHONY: build test fmt vet tidy clean release

build:
	go build $(LDFLAGS) -o bin/runlog-verifier ./cmd/runlog-verifier

# Cross-compile all four release targets locally — same flags as the build
# target, same flags as .github/workflows/release.yml. Useful for smoking
# the workflow's Go invocations without needing a tag push.
release:
	@set -e; \
	for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
		goos=$${t%/*}; goarch=$${t#*/}; \
		name=runlog-verifier-$$goos-$$goarch; \
		echo "build $$name"; \
		GOOS=$$goos GOARCH=$$goarch \
			go build $(LDFLAGS) -o dist/$$name ./cmd/runlog-verifier; \
	done; \
	cd dist && sha256sum runlog-verifier-* | sort -k2 > SHA256SUMS; \
	cat SHA256SUMS

test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin/ dist/
