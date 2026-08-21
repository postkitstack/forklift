BINARY  := bin/forklift
PKG     := ./cmd/forklift
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GO      ?= go
PREFIX  ?= /usr/local

.PHONY: help build install test test-integration vet fmt clean doctor

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into bin/
	@mkdir -p bin
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) $(PKG)
	@echo "built $(BINARY) ($(VERSION))"

install: build ## Install to $(PREFIX)/bin (on sudo's secure_path)
	@if [ "$$(id -u)" = 0 ]; then \
		install -m 0755 $(BINARY) $(PREFIX)/bin/forklift; \
	else \
		sudo install -m 0755 $(BINARY) $(PREFIX)/bin/forklift; \
	fi
	@echo "installed $(PREFIX)/bin/forklift"

test: ## Unit tests (no root needed)
	$(GO) test ./...

# Run WITHOUT sudo: this recipe elevates itself, forwarding PATH, because sudo
# resets PATH to secure_path and would lose the Go toolchain.
test-integration: ## Storage conformance suite (needs btrfs-progs; elevates itself)
	@command -v $(GO) >/dev/null 2>&1 || { \
		echo "go not found on PATH."; \
		echo "If you typed 'sudo make', run 'make test-integration' instead —"; \
		echo "the recipe elevates itself and sudo strips Go from PATH."; \
		exit 1; }
	@if [ "$$(id -u)" = 0 ]; then \
		$(GO) test ./internal/storage/ -run Conformance -v; \
	else \
		echo "Elevating for the pool (loop devices, mount)..."; \
		sudo -E env "PATH=$$PATH" $(GO) test ./internal/storage/ -run Conformance -v; \
	fi

vet: ## go vet
	$(GO) vet ./...

fmt: ## Format
	$(GO) fmt ./...

doctor: build ## Report this machine's COW capabilities
	sudo $(BINARY) doctor

clean: ## Remove build output
	rm -rf bin
