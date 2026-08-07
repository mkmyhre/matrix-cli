GO ?= go
GOFMT ?= gofmt
TAGS ?= goolm
BINARY ?= matrix
PACKAGE := ./cmd/matrix

.PHONY: all build install test test-no-encryption vet fmt fmt-check mod-verify check clean help

all: build

## Build an encryption-enabled binary in the current directory.
build:
	$(GO) build -tags "$(TAGS)" -o "$(BINARY)" $(PACKAGE)

## Install into GOBIN (or GOPATH/bin when GOBIN is unset).
install:
	$(GO) install -tags "$(TAGS)" $(PACKAGE)

## Run the complete test suite with encryption support enabled.
test:
	$(GO) test -tags "$(TAGS)" ./...

## Also ensure the build-tag fallback remains healthy.
test-no-encryption:
	$(GO) test ./...

vet:
	$(GO) vet -tags "$(TAGS)" ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@test -z "$$($(GOFMT) -l $$(find . -name '*.go' -not -path './.git/*'))" || { \
		echo 'Go files are not formatted; run make fmt'; exit 1; \
	}

mod-verify:
	$(GO) mod verify

check: fmt-check mod-verify test-no-encryption test vet

clean:
	rm -f "$(BINARY)"

help:
	@printf '%s\n' \
		'make build    Build ./matrix with end-to-end encryption support' \
		'make install  Install matrix into GOBIN or GOPATH/bin' \
		'make test     Run tests with encryption support' \
		'make test-no-encryption  Test the encryption-disabled fallback' \
		'make vet      Run Go static analysis' \
		'make fmt      Format Go source files' \
		'make check    Verify formatting/modules and run all tests and vet' \
		'make clean    Remove the local binary' \
		'' \
		'Override defaults with TAGS=..., BINARY=..., GO=..., or GOFMT=...'
