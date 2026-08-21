BINARY  := bin/forklift
PKG     := ./cmd/forklift
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# sudo resets PATH to secure_path, which excludes /usr/local/go/bin, so plain
# "sudo make test-integration" would die in the build prerequisite with
# "go: not found". Fall back to the absolute path when go is not on PATH.
GO      ?= $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)

.PHONY: help build install test test-integration vet fmt clean doctor

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	@mkdir -p bin
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) $(PKG)
	@echo "built $(BINARY) ($(VERSION))"

install: ## Install to GOBIN
	$(GO) install -ldflags "-X main.version=$(VERSION)" $(PKG)

test: ## Unit tests (no root needed)
	$(GO) test ./...

test-integration: build ## Storage conformance suite (needs root + btrfs-progs)
	@echo "Running as root — the pool needs loop devices and mount."
	sudo -E env "PATH=$$PATH" $(GO) test ./internal/storage/ -run Conformance -v

vet: ## go vet
	$(GO) vet ./...

fmt: ## Format
	$(GO) fmt ./...

doctor: build ## Report this machine's COW capabilities
	sudo $(BINARY) doctor

clean: ## Remove build output
	rm -rf bin
