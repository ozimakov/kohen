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

# Image names/tags for local e2e; overridable in CI.
IMG ?= kohen:e2e
GITSERVER_IMG ?= kohen-e2e-gitserver:e2e
KIND_CLUSTER ?= kohen

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

##@ End-to-end (Tier 3, kind)

.PHONY: docker-build
docker-build: ## Build the operator and e2e gitserver images.
	docker build -t $(IMG) -f Dockerfile .
	docker build -t $(GITSERVER_IMG) -f test/e2e/gitserver/Dockerfile .

.PHONY: image
image: ## Build multi-arch operator images (amd64 + arm64) via buildx.
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t $(IMG) -f Dockerfile --load .

.PHONY: manifests-bundle
manifests-bundle: ## Render plain Kubernetes manifests from the Helm chart.
	mkdir -p deploy/manifests
	helm template kohen deploy/helm/kohen --include-crds \
		--namespace kohen-system > deploy/manifests/kohen.yaml
	helm template kohen deploy/helm/kohen --include-crds \
		--namespace kohen-system --set scope=namespaced \
		> deploy/manifests/kohen-namespaced.yaml

.PHONY: kind-load
kind-load: ## Load the built images into the kind cluster.
	kind load docker-image $(IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(GITSERVER_IMG) --name $(KIND_CLUSTER)

.PHONY: e2e
e2e: ## Run the U1 config e2e suite (requires a kind cluster with Kohen installed and images loaded).
	GITSERVER_IMAGE=$(GITSERVER_IMG) $(GO) test -tags e2e -count=1 -timeout 20m -v -run '^TestU1' ./test/e2e/...

.PHONY: e2e-secrets
e2e-secrets: ## Run the U2 secret-integration e2e suite (requires kind + Kohen + ESO installed; see .github/workflows/e2e.yml).
	GITSERVER_IMAGE=$(GITSERVER_IMG) $(GO) test -tags e2e -count=1 -timeout 25m -v -run '^TestU2' ./test/e2e/...

.PHONY: e2e-security
e2e-security: ## Run S3.1 security conformance tests (A9).
	GITSERVER_IMAGE=$(GITSERVER_IMG) $(GO) test -tags e2e -count=1 -timeout 15m -v -run '^TestU3(PodSecurity|RBAC|Namespaced)' ./test/e2e/...

.PHONY: e2e-acceptance
e2e-acceptance: ## Run U3 acceptance additions (A2 matrix doc + mount content).
	GITSERVER_IMAGE=$(GITSERVER_IMG) $(GO) test -tags e2e -count=1 -timeout 15m -v -run '^TestU3(Acceptance|Mounted)' ./test/e2e/...

.PHONY: e2e-lifecycle
e2e-lifecycle: ## Run A12 upgrade test (set KOHEN_ALLOW_UNINSTALL=true for uninstall).
	GITSERVER_IMAGE=$(GITSERVER_IMG) KOHEN_IMAGE=$(IMG) KOHEN_CHART_PATH=$(CURDIR)/deploy/helm/kohen $(GO) test -tags e2e -count=1 -timeout 20m -v -run '^TestU3Operator' ./test/e2e/...

.PHONY: e2e-u3
e2e-u3: ## Run the full U3 acceptance gate (U1 + U2 + security + acceptance + upgrade).
	$(MAKE) e2e
	$(MAKE) e2e-secrets
	$(MAKE) e2e-security
	$(MAKE) e2e-acceptance
	$(MAKE) e2e-lifecycle

.PHONY: verify-docs
verify-docs: ## Validate SPEC refs in PLAN.md and doc links.
	bash scripts/verify-spec-refs.sh
	bash scripts/verify-doc-links.sh

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
verify: tidy vet build test verify-docs ## Run the full local verification suite.
	@git diff --exit-code go.mod go.sum
