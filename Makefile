# kube-rightsize Makefile

# Image
IMG ?= ghcr.io/sebtardiflabs/kube-rightsize:latest

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
CERT_MANAGER_VERSION ?= v1.17.2

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

SHELL = /usr/bin/env bash -Eeuo pipefail
.SHELLFLAGS = -c

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(BUILD_DATE)

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

.PHONY: verify-doc-defaults
verify-doc-defaults: ## Verify critical defaults are consistent across docs and code
	@bash hack/verify-doc-defaults.sh

.PHONY: verify-helm-rbac
verify-helm-rbac: ## Verify Helm chart ClusterRole matches kustomize RBAC
	@bash hack/verify-helm-rbac.sh

.PHONY: verify-dashboard-metrics
verify-dashboard-metrics: ## Verify Helm dashboard stays synced with the standalone source dashboard
	@bash hack/verify-dashboard-metrics.sh

.PHONY: verify-doc-tool-versions
verify-doc-tool-versions: ## Verify supported tool version references stay consistent in docs
	@bash hack/verify-doc-tool-versions.sh

.PHONY: verify-quick
verify-quick: lint yaml-lint test helm-lint helm-docs-check helm-unittest verify-boilerplate tidy-check verify-doc-defaults verify-helm-rbac verify-dashboard-metrics verify-doc-tool-versions ## Fast pre-commit checks (no integration tests or govulncheck)

.PHONY: verify
verify: verify-quick test-integration govulncheck ## Run all CI checks locally (includes integration tests)
	@$(MAKE) manifests generate
	@git diff --quiet --exit-code config/crd/ charts/kube-rightsize/crds/ api/v1alpha1/zz_generated.deepcopy.go config/rbac/ || \
		(echo "::error::Generated files are stale. Run 'make manifests generate' and commit." && exit 1)

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin/ dist/ coverage.out

.PHONY: docs-build
docs-build: ## Build documentation site (requires pip install mkdocs-material)
	mkdocs build --strict

.PHONY: docs-serve
docs-serve: ## Serve documentation site locally (requires pip install mkdocs-material)
	mkdocs serve

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
	@command -v yamllint >/dev/null 2>&1 || { echo "Installing yamllint..."; pip install --user --break-system-packages yamllint 2>/dev/null || pip install --user yamllint; }
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
test-e2e: chainsaw ## Run Chainsaw E2E tests (requires a pre-provisioned k3d or Kind cluster)
	$(CHAINSAW) test test/e2e/ --config .chainsaw.yaml

.PHONY: test-e2e-go
test-e2e-go: ## Run Go E2E tests (requires a pre-provisioned k3d/Kind cluster with operator + Prometheus)
	go test -tags=e2e ./test/e2e-go/... -race -count=1 -timeout=15m -v

.PHONY: test-e2e-smoke
test-e2e-smoke: chainsaw ## Run a minimal E2E smoke suite (requires a pre-provisioned k3d/Kind cluster with operator + Prometheus)
	$(CHAINSAW) test test/e2e/oneshot-resize --config .chainsaw.yaml
	go test -tags=e2e ./test/e2e-go/... -run '^TestE2E_OneShotMode_ResizesOnePod$$' -race -count=1 -timeout=10m -v

.PHONY: test-fuzz
test-fuzz: ## Run fuzz tests (30 seconds per target)
	go test ./internal/recommendation/... -run='^$$' -fuzz=FuzzPercentileEstimator -fuzztime=30s
	go test ./internal/recommendation/... -run='^$$' -fuzz=FuzzRecommendationEngine -fuzztime=30s

.PHONY: test-bench
test-bench: ## Run benchmark tests
	go test ./internal/... -bench=. -benchmem -run=^$$ -timeout=5m

.PHONY: test-local
test-local: test test-integration ## Run unit + integration + Chainsaw E2E + Go E2E with an auto-provisioned k3d cluster
	@cluster_name=kube-rightsize-test; \
	trap 'k3d cluster delete "$$cluster_name" 2>/dev/null || true' EXIT; \
	k3d cluster delete "$$cluster_name" 2>/dev/null || true; \
	k3d cluster create "$$cluster_name" \
		--image rancher/k3s:$(K3S_VERSION) \
		--k3s-arg "--disable=traefik,servicelb@server:*" \
		--wait --timeout 120s; \
	$(MAKE) docker-build IMG=kube-rightsize:test; \
	k3d image import kube-rightsize:test -c "$$cluster_name"; \
	$(MAKE) _deploy-stack IMG=kube-rightsize:test; \
	$(MAKE) test-e2e; \
	$(MAKE) test-e2e-go

.PHONY: test-local-smoke
test-local-smoke: ## Provision k3d, deploy the operator stack, run the minimal E2E smoke suite, and clean up
	@cluster_name=krsmoke; \
	trap 'k3d cluster delete "$$cluster_name" 2>/dev/null || true' EXIT; \
	k3d cluster delete "$$cluster_name" 2>/dev/null || true; \
	k3d cluster create "$$cluster_name" \
		--image rancher/k3s:$(K3S_VERSION) \
		--k3s-arg "--disable=traefik,servicelb@server:*" \
		--wait --timeout 120s; \
	$(MAKE) docker-build IMG=kube-rightsize:test; \
	k3d image import kube-rightsize:test -c "$$cluster_name"; \
	$(MAKE) _deploy-stack IMG=kube-rightsize:test; \
	$(MAKE) test-e2e-smoke

.PHONY: test-all
test-all: test test-integration test-e2e test-e2e-go ## Run all tests (E2E requires a pre-provisioned cluster with operator + Prometheus; see CONTRIBUTING.md)

##@ Build

.PHONY: build
build: manifests generate ## Build operator binary
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/manager ./cmd/manager/

.PHONY: build-plugin
build-plugin: ## Build kubectl-rightsize plugin
	go build -trimpath -ldflags="$(LDFLAGS)" -o bin/kubectl-rightsize ./cmd/kubectl-rightsize/

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

# k3d settings (lightweight, fast startup)
K3D_CLUSTER_NAME ?= kube-rightsize
K3S_VERSION ?= v1.35.4-k3s1

# Kind settings (upstream K8s, production-accurate)
KIND_CLUSTER_NAME ?= kube-rightsize
KIND_NODE_IMAGE ?= kindest/node:v1.33.7

.PHONY: k3d-create
k3d-create: ## Create a k3d cluster for local dev (fast, uses k3s)
	k3d cluster create $(K3D_CLUSTER_NAME) \
		--image rancher/k3s:$(K3S_VERSION) \
		--k3s-arg "--disable=traefik,servicelb@server:*" \
		--wait --timeout 120s

.PHONY: k3d-delete
k3d-delete: ## Delete the k3d cluster
	k3d cluster delete $(K3D_CLUSTER_NAME)

# Internal: install cert-manager, Prometheus, and operator (called after image load)
_deploy-stack:
	@echo "Installing cert-manager..."
	kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml
	kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=120s
	@echo "Installing Prometheus..."
	helm repo add prometheus-community https://prometheus-community.github.io/helm-charts 2>/dev/null || true
	helm repo update
	helm install prometheus prometheus-community/prometheus \
		--version 27.22.0 \
		--namespace monitoring --create-namespace \
		--set server.persistentVolume.enabled=false \
		--set alertmanager.enabled=false \
		--set prometheus-pushgateway.enabled=false \
		--set server.global.scrape_interval=15s \
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

.PHONY: k3d-deploy
k3d-deploy: docker-build ## Build, load, and deploy to k3d (with Prometheus + cert-manager)
	k3d image import $(IMG) -c $(K3D_CLUSTER_NAME)
	@$(MAKE) _deploy-stack

.PHONY: kind-create
kind-create: ## Create a Kind cluster for local dev (upstream K8s)
	kind create cluster --name $(KIND_CLUSTER_NAME) --image $(KIND_NODE_IMAGE) --wait 120s

.PHONY: kind-delete
kind-delete: ## Delete the Kind cluster
	kind delete cluster --name $(KIND_CLUSTER_NAME)

.PHONY: kind-deploy
kind-deploy: docker-build ## Build, load, and deploy to Kind (with Prometheus + cert-manager)
	kind load docker-image $(IMG) --name $(KIND_CLUSTER_NAME)
	@$(MAKE) _deploy-stack

##@ Release

.PHONY: build-installer
build-installer: manifests kustomize ## Generate install manifest for release
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default > dist/install.yaml

.PHONY: build-crds
build-crds: manifests ## Generate standalone CRDs bundle for manual upgrades
	mkdir -p dist
	cat charts/kube-rightsize/crds/*.yaml > dist/crds.yaml

##@ Tools

# Version-aware tool installs: embed version in binary filename so a version
# bump in the Makefile automatically triggers a fresh install.
LOCALBIN ?= $(GOBIN)

CONTROLLER_GEN = $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
.PHONY: controller-gen
controller-gen: ## Install controller-gen
	@test -s $(CONTROLLER_GEN) || { \
		go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION) && \
		mv $(LOCALBIN)/controller-gen $(CONTROLLER_GEN); \
	}

GOTESTSUM = $(LOCALBIN)/gotestsum-$(GOTESTSUM_VERSION)
.PHONY: gotestsum
gotestsum: ## Install gotestsum
	@test -s $(GOTESTSUM) || { \
		go install gotest.tools/gotestsum@$(GOTESTSUM_VERSION) && \
		mv $(LOCALBIN)/gotestsum $(GOTESTSUM); \
	}

SETUP_ENVTEST = $(LOCALBIN)/setup-envtest
.PHONY: setup-envtest
setup-envtest: ## Install setup-envtest
	@test -s $(SETUP_ENVTEST) || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.24

GOLANGCI_LINT = $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
.PHONY: golangci-lint
golangci-lint: ## Install golangci-lint
	@test -s $(GOLANGCI_LINT) || { \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) && \
		mv $(LOCALBIN)/golangci-lint $(GOLANGCI_LINT); \
	}

KUSTOMIZE = $(LOCALBIN)/kustomize-$(KUSTOMIZE_VERSION)
.PHONY: kustomize
kustomize: ## Install kustomize
	@test -s $(KUSTOMIZE) || { \
		go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION) && \
		mv $(LOCALBIN)/kustomize $(KUSTOMIZE); \
	}

CHAINSAW = $(LOCALBIN)/chainsaw-$(CHAINSAW_VERSION)
.PHONY: chainsaw
chainsaw: ## Install chainsaw
	@test -s $(CHAINSAW) || { \
		go install github.com/kyverno/chainsaw@$(CHAINSAW_VERSION) && \
		mv $(LOCALBIN)/chainsaw $(CHAINSAW); \
	}

HELM_DOCS = $(LOCALBIN)/helm-docs-$(HELM_DOCS_VERSION)
.PHONY: helm-docs
helm-docs: ## Install helm-docs
	@test -s $(HELM_DOCS) || { \
		go install github.com/norwoodj/helm-docs/cmd/helm-docs@$(HELM_DOCS_VERSION) && \
		mv $(LOCALBIN)/helm-docs $(HELM_DOCS); \
	}

.PHONY: helm-lint
helm-lint: ## Lint Helm chart and validate templates (mirrors CI helm-lint job)
	helm lint charts/kube-rightsize --kube-version v1.33.0
	@for f in charts/kube-rightsize/ci/*.yaml; do \
		echo "--- Template validation with $$f ---"; \
		helm template kube-rightsize charts/kube-rightsize -f "$$f" \
			--kube-version v1.33.0 \
			--api-versions cert-manager.io/v1 > /dev/null; \
	done

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
