.PHONY: all build build-smartctl test lint clean help

BINARY_NAME=pve-exporter
SMARTCTL_BINARY_NAME=smartctl-exporter

all: lint test build

build:
	go build -o $(BINARY_NAME) -v

build-smartctl:
	go build -o $(SMARTCTL_BINARY_NAME) -v ./agents/smartctl-exporter

test:
	go test -v -race ./...

lint:
	golangci-lint run

clean:
	go clean
	rm -f $(BINARY_NAME)

help:
	@echo "Makefile commands:"
	@echo "  make build    - Build the binary"
	@echo "  make build-smartctl - Build the smartctl exporter binary"
	@echo "  make test     - Run tests"
	@echo "  make lint     - Run linter"
	@echo "  make clean    - Clean build artifacts"
	@echo "  make all      - Run lint, test, and build"
