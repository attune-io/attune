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
GOTESTSUM_VERSION ?= v1.13.0
GOVULNCHECK_VERSION ?= v1.3.0
K3D_VERSION ?= v5.8.3
GITLEAKS_VERSION ?= 8.30.1

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
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: verify-boilerplate
verify-boilerplate: ## Check license headers on all Go files
	@missing=$$(find . -name '*.go' -not -path './vendor/*' -not -name 'zz_generated.*' \
	  -exec sh -c 'head -5 "$$1" | grep -q "^Copyright" || echo "$$1"' _ {} \;); \
	if [ -n "$$missing" ]; then echo "Missing license header:" && echo "$$missing" && exit 1; fi

.PHONY: govulncheck
govulncheck: ## Run govulncheck for known vulnerabilities
	@test -s $(GOBIN)/govulncheck || go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	$(GOBIN)/govulncheck ./...

.PHONY: tidy-check
tidy-check: ## Verify go.mod/go.sum are tidy
	go mod tidy
	@git diff --quiet --exit-code go.mod go.sum || \
		(echo "::error::go.mod/go.sum are not tidy. Run 'go mod tidy' and commit." && exit 1)

.PHONY: verify
verify: lint yaml-lint test helm-docs-check helm-unittest verify-boilerplate tidy-check ## Run all CI checks locally
	@$(MAKE) manifests
	@git diff --quiet --exit-code config/crd/ charts/kube-rightsize/crds/ || \
		(echo "::error::CRD manifests are stale. Run 'make manifests' and commit." && exit 1)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/ coverage.out

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

.PHONY: yaml-lint
yaml-lint: ## Lint YAML files (mirrors CI)
	@command -v yamllint >/dev/null 2>&1 || { echo "Install yamllint: pip install yamllint"; exit 1; }
	@yamllint -d '{extends: default, rules: {line-length: {max: 200}, truthy: {check-keys: false}, indentation: {spaces: 2, indent-sequences: whatever}}}' \
		config/ charts/kube-rightsize/Chart.yaml charts/kube-rightsize/values.yaml charts/kube-rightsize/ci/

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint with auto-fix
	$(GOLANGCI_LINT) run --fix --timeout 5m

##@ Testing

.PHONY: test
test: manifests generate gotestsum ## Run unit tests
	$(GOTESTSUM) --format pkgname \
		--rerun-fails --rerun-fails-max-failures=5 \
		--packages="./api/... ./cmd/... ./internal/..." \
		-- -race -timeout=10m \
		-coverpkg=./internal/... \
		-coverprofile=coverage.out \
		-covermode=atomic
	@echo ""
	@go tool cover -func=coverage.out | grep total:

.PHONY: test-integration
test-integration: manifests generate setup-envtest gotestsum ## Run integration tests
	KUBEBUILDER_ASSETS="$(shell $(SETUP_ENVTEST) use -p path)" \
		$(GOTESTSUM) --format pkgname \
		--rerun-fails --rerun-fails-max-failures=3 \
		--packages="./test/integration/..." \
		-- -race -count=1 -timeout=15m -tags=integration

.PHONY: test-e2e
test-e2e: chainsaw ## Run E2E tests (requires Kind cluster)
	$(CHAINSAW) test test/e2e/ --config .chainsaw.yaml

.PHONY: test-fuzz
test-fuzz: ## Run fuzz tests (30 seconds per target)
	go test ./internal/recommendation/... -run='^$$' -fuzz=FuzzPercentileEstimator -fuzztime=30s
	go test ./internal/recommendation/... -run='^$$' -fuzz=FuzzRecommendationEngine -fuzztime=30s

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

K3D_CLUSTER_NAME ?= kube-rightsize
K3S_VERSION ?= v1.33.11-k3s1

.PHONY: k3d-create
k3d-create: ## Create a k3d cluster for local dev
	k3d cluster create $(K3D_CLUSTER_NAME) \
		--image rancher/k3s:$(K3S_VERSION) \
		--k3s-arg "--disable=traefik,servicelb@server:*" \
		--wait --timeout 120s

.PHONY: k3d-delete
k3d-delete: ## Delete the k3d cluster
	k3d cluster delete $(K3D_CLUSTER_NAME)

.PHONY: k3d-deploy
k3d-deploy: docker-build ## Build, load, and deploy to k3d (with Prometheus + cert-manager)
	k3d image import $(IMG) -c $(K3D_CLUSTER_NAME)
	@echo "Installing cert-manager..."
	kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.17.2/cert-manager.yaml
	kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=120s
	@echo "Installing Prometheus..."
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
	helm repo update
	helm install prometheus prometheus-community/prometheus \
		--namespace monitoring --create-namespace \
		--set server.persistentVolume.enabled=false \
		--set alertmanager.enabled=false \
		--set prometheus-pushgateway.enabled=false \
		--wait --timeout 3m 2>/dev/null || true
	@echo "Installing operator via Helm..."
	helm install kube-rightsize ./charts/kube-rightsize \
		--namespace kube-rightsize-system --create-namespace \
		--set image.repository=$(firstword $(subst :, ,$(IMG))) \
		--set image.tag=$(lastword $(subst :, ,$(IMG))) \
		--set image.pullPolicy=Never \
		--set webhooks.enabled=true \
		--set metrics.enabled=true \
		--set leaderElection.enabled=false \
		--wait --timeout 3m

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

GOTESTSUM = $(GOBIN)/gotestsum
.PHONY: gotestsum
gotestsum: ## Install gotestsum
	@test -s $(GOTESTSUM) || go install gotest.tools/gotestsum@$(GOTESTSUM_VERSION)

SETUP_ENVTEST = $(GOBIN)/setup-envtest
.PHONY: setup-envtest
setup-envtest: ## Install setup-envtest
	@test -s $(SETUP_ENVTEST) || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.24

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

HELM_DOCS = $(GOBIN)/helm-docs
.PHONY: helm-docs
helm-docs: ## Install helm-docs
	@test -s $(HELM_DOCS) || go install github.com/norwoodj/helm-docs/cmd/helm-docs@$(HELM_DOCS_VERSION)

.PHONY: helm-docs-gen
helm-docs-gen: helm-docs ## Generate Helm chart README from values.yaml
	$(HELM_DOCS) --chart-search-root charts/

.PHONY: helm-docs-check
helm-docs-check: helm-docs-gen ## Verify Helm docs are up to date
	@git diff --quiet --exit-code charts/kube-rightsize/README.md || \
		(echo "::error::Helm README is stale. Run 'make helm-docs-gen' and commit." && exit 1)

.PHONY: helm-unittest
helm-unittest: ## Run Helm chart unit tests
	@helm plugin list | grep -q unittest || helm plugin install https://github.com/helm-unittest/helm-unittest.git --verify=false
	helm unittest charts/kube-rightsize
