# Kohen — developer Makefile
#
# Common entry points used by CI and local development. See PLAN.md S0.1.
# Additional targets (image, manifests, e2e) are introduced by later plan steps.

SHELL := /usr/bin/env bash
GO ?= go
GOBIN ?= $(shell $(GO) env GOPATH)/bin

GOLANGCI_LINT_VERSION ?= v1.61.0
GOLANGCI_LINT ?= $(GOBIN)/golangci-lint

CONTROLLER_GEN_VERSION ?= v0.16.5
CONTROLLER_GEN ?= $(GOBIN)/controller-gen

ENVTEST_VERSION ?= release-0.19
ENVTEST ?= $(GOBIN)/setup-envtest
ENVTEST_K8S_VERSION ?= 1.31.0

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

##@ Code generation

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy code.
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths=./api/...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate CRD manifests + RBAC role and sync CRD into the Helm chart.
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=kohen-manager-role paths=./internal/controller/... output:rbac:artifacts:config=config/rbac
	mkdir -p deploy/helm/kohen/crds
	cp config/crd/bases/*.yaml deploy/helm/kohen/crds/

$(CONTROLLER_GEN):
	GOBIN=$(GOBIN) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

$(ENVTEST):
	GOBIN=$(GOBIN) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: envtest-assets
envtest-assets: $(ENVTEST) ## Print the KUBEBUILDER_ASSETS path (downloads envtest binaries).
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path

##@ Testing

.PHONY: test
test: ## Run unit tests with the race detector (envtest tiers skip without assets).
	$(GO) test -race -count=1 ./...

.PHONY: test-integration
test-integration: $(ENVTEST) ## Run all tests including envtest (Tier 2).
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" $(GO) test -race -count=1 ./...

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
