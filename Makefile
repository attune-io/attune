# kube-rightsize Makefile

# Image
IMG ?= ghcr.io/sebtardif/kube-rightsize:latest

# Tool versions
KUBEBUILDER_VERSION ?= 4.14.0
CONTROLLER_TOOLS_VERSION ?= v0.17.0
GOLANGCI_LINT_VERSION ?= v2.12.2
CHAINSAW_VERSION ?= v0.2.15
KUSTOMIZE_VERSION ?= v5.6.0
HELM_DOCS_VERSION ?= v1.14.2

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -Eeuo pipefail
.SHELLFLAGS = -c

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests, RBAC, and webhook configs
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases output:rbac:artifacts:config=config/rbac
	cp config/crd/bases/*.yaml charts/kube-rightsize/crds/

.PHONY: generate
generate: controller-gen ## Generate deepcopy methods
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt
	go fmt ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint
	$(GOLANGCI_LINT) run --timeout 5m

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint with auto-fix
	$(GOLANGCI_LINT) run --fix --timeout 5m

##@ Testing

.PHONY: test
test: manifests generate ## Run unit tests
	go test ./api/... ./internal/... -race -count=1 -coverprofile=coverage.out -covermode=atomic
	@echo ""
	@go tool cover -func=coverage.out | grep total:

.PHONY: test-integration
test-integration: manifests generate setup-envtest ## Run integration tests
	KUBEBUILDER_ASSETS="$(shell $(SETUP_ENVTEST) use -p path)" \
		go test ./test/integration/... -race -count=1 -timeout=15m -tags=integration

.PHONY: test-e2e
test-e2e: chainsaw ## Run E2E tests (requires Kind cluster)
	$(CHAINSAW) test test/e2e/ --config .chainsaw.yaml

.PHONY: test-fuzz
test-fuzz: ## Run fuzz tests (30 seconds per target)
	go test ./internal/recommendation/... -fuzz=. -fuzztime=30s

.PHONY: test-bench
test-bench: ## Run benchmark tests
	go test ./internal/... -bench=. -benchmem -run=^$$ -timeout=5m

.PHONY: test-all
test-all: test test-integration test-e2e ## Run all tests

##@ Build

.PHONY: build
build: manifests generate ## Build operator binary
	go build -o bin/manager ./cmd/manager/

.PHONY: build-plugin
build-plugin: ## Build kubectl-rightsize plugin
	go build -o bin/kubectl-rightsize ./cmd/kubectl-rightsize/

.PHONY: run
run: manifests generate ## Run operator locally against the configured cluster
	go run ./cmd/manager/

.PHONY: docker-build
docker-build: ## Build container image
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push container image
	docker push $(IMG)

##@ Deployment

.PHONY: install
install: manifests ## Install CRDs into the cluster
	kubectl apply -f config/crd/bases/

.PHONY: uninstall
uninstall: ## Uninstall CRDs from the cluster
	kubectl delete -f config/crd/bases/ --ignore-not-found

.PHONY: deploy
deploy: manifests kustomize ## Deploy operator to the cluster
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	kubectl create namespace kube-rightsize-system --dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -k config/default/

.PHONY: undeploy
undeploy: ## Undeploy operator from the cluster
	kubectl delete -k config/default/ --ignore-not-found

##@ Local Development

.PHONY: kind-create
kind-create: ## Create a Kind cluster for local dev
	kind create cluster --name kube-rightsize --image kindest/node:v1.35.0 || true

.PHONY: kind-delete
kind-delete: ## Delete the Kind cluster
	kind delete cluster --name kube-rightsize

.PHONY: kind-deploy
kind-deploy: docker-build ## Build, load, and deploy to Kind
	kind load docker-image $(IMG) --name kube-rightsize
	$(MAKE) install deploy

##@ Release

.PHONY: build-installer
build-installer: manifests kustomize ## Generate install manifest for release
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Tools

CONTROLLER_GEN = $(GOBIN)/controller-gen
.PHONY: controller-gen
controller-gen: ## Install controller-gen
	@test -s $(CONTROLLER_GEN) || go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

SETUP_ENVTEST = $(GOBIN)/setup-envtest
.PHONY: setup-envtest
setup-envtest: ## Install setup-envtest
	@test -s $(SETUP_ENVTEST) || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

GOLANGCI_LINT = $(GOBIN)/golangci-lint
.PHONY: golangci-lint
golangci-lint: ## Install golangci-lint
	@test -s $(GOLANGCI_LINT) || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

KUSTOMIZE = $(GOBIN)/kustomize
.PHONY: kustomize
kustomize: ## Install kustomize
	@test -s $(KUSTOMIZE) || go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

CHAINSAW = $(GOBIN)/chainsaw
.PHONY: chainsaw
chainsaw: ## Install chainsaw
	@test -s $(CHAINSAW) || go install github.com/kyverno/chainsaw@$(CHAINSAW_VERSION)

.PHONY: helm-unittest
helm-unittest: ## Run Helm chart unit tests
	@helm plugin list | grep -q unittest || helm plugin install https://github.com/helm-unittest/helm-unittest.git --verify=false
	helm unittest charts/kube-rightsize
