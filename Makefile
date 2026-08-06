GO ?= go
TAGS ?= goolm
BINARY ?= matrix
PACKAGE := ./cmd/matrix

.PHONY: all build install test vet fmt check clean help

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

vet:
	$(GO) vet -tags "$(TAGS)" ./...

fmt:
	$(GO) fmt ./...

check: test vet

clean:
	rm -f "$(BINARY)"

help:
	@printf '%s\n' \
		'make build    Build ./matrix with end-to-end encryption support' \
		'make install  Install matrix into GOBIN or GOPATH/bin' \
		'make test     Run tests with encryption support' \
		'make vet      Run Go static analysis' \
		'make fmt      Format Go source files' \
		'make check    Run tests and static analysis' \
		'make clean    Remove the local binary' \
		'' \
		'Override defaults with TAGS=..., BINARY=..., or GO=...'
