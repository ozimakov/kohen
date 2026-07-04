# Kohen — developer Makefile
#
# Common entry points used by CI and local development. See PLAN.md S0.1.
# Additional targets (image, manifests, e2e) are introduced by later plan steps.

SHELL := /usr/bin/env bash
GO ?= go
GOBIN ?= $(shell $(GO) env GOPATH)/bin

GOLANGCI_LINT_VERSION ?= v1.61.0
GOLANGCI_LINT ?= $(GOBIN)/golangci-lint

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: tidy
tidy: ## Ensure go.mod/go.sum are tidy.
	$(GO) mod tidy

.PHONY: build
build: ## Build all packages.
	$(GO) build ./...

.PHONY: test
test: ## Run unit and integration tests with the race detector.
	$(GO) test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and produce a coverage profile.
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint.
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: $(GOLANGCI_LINT) ## Run golangci-lint with --fix.
	$(GOLANGCI_LINT) run --fix

$(GOLANGCI_LINT):
	GOBIN=$(GOBIN) $(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: verify
verify: tidy vet build test ## Run the full local verification suite.
	@git diff --exit-code go.mod go.sum
