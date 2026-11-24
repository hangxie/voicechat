.DEFAULT_GOAL=help

# Required for globs to work correctly
SHELL:=/bin/bash

BUILD_DIR	= $(CURDIR)/build
PKG_PREFIX	= github.com/hangxie/voicechat

# go option
CGO_ENABLED := 0
GO			?= go
GOBIN		= $(shell $(GO) env GOPATH)/bin
GOFLAGS		:= -trimpath
GOSOURCES	:= $(shell find . -type f -name '*.go' -not -path './build/*')
LDFLAGS		:= -w -s

# Backends for `make run`; override any of them on the command line or in the environment.
STT_URL		?= http://localhost:5005/v1
LLM_URL		?= http://localhost:8080/v1
TTS_URL		?= http://localhost:5004
VOICE_CHAT_HOST	?= 127.0.0.1
VOICE_CHAT_PORT	?= 5006

.EXPORT_ALL_VARIABLES:

.PHONY: all
all: deps tools format lint test build  ## Build all common targets

.PHONY: format
format: tools  ## Format source codes
	@echo "==> Formatting source codes"
	@$(GOBIN)/gofumpt -w -extra $(GOSOURCES)
	@$(GOBIN)/goimports -w -local $(PKG_PREFIX) $(GOSOURCES)

.PHONY: lint
lint: tools  ## Run static code analysis
	@echo "==> Running static code analysis"
	@$(GOBIN)/golangci-lint cache clean
	@$(GOBIN)/golangci-lint run ./... --timeout 5m --enable gocognit

.PHONY: deps
deps:  ## Install prerequisite for build
	@echo "==> Installing prerequisite for build"
	@$(GO) mod tidy

.PHONY: tools
tools:  ## Install build tools
	@echo "==> Installing build tools"
	@(cd /tmp; \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0; \
		$(GO) install mvdan.cc/gofumpt@latest; \
		$(GO) install golang.org/x/tools/cmd/goimports@latest; \
	)

.PHONY: build
build: deps  ## Build locally for local os/arch creating $(BUILD_DIR) in ./
	@echo "==> Building executable"
	@mkdir -p $(BUILD_DIR)
	@CGO_ENABLED=$(CGO_ENABLED) \
		$(GO) build $(GOFLAGS) \
			-ldflags '$(LDFLAGS)' \
			-o $(BUILD_DIR) ./

.PHONY: run
run: build  ## Run locally against STT_URL, LLM_URL, and TTS_URL (open http://localhost:5006/)
	@echo "==> Running on $(VOICE_CHAT_HOST):$(VOICE_CHAT_PORT)"
	@$(BUILD_DIR)/voice-chat

.PHONY: test
test: deps tools  ## Run unit tests
	@echo "==> Running unit tests"
	@mkdir -p $(BUILD_DIR)/test
	@set -euo pipefail ; \
		cd $(BUILD_DIR)/test; \
		CGO_ENABLED=1 $(GO) test -parallel 4 -race -count 1 -trimpath -coverprofile=coverage.out $(CURDIR)/... ; \
		$(GO) tool cover -html=coverage.out -o coverage.html ; \
		$(GO) tool cover -func=coverage.out -o coverage.txt ; \
		cat coverage.txt

.PHONY: clean
clean:  ## Clean up the build dirs
	@echo "==> Cleaning up build dirs"
	@rm -rf $(BUILD_DIR) vendor

.PHONY: help
help:  ## Print list of Makefile targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  cut -d ":" -f1- | \
	  awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'
