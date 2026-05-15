# Makefile for production-stack

PROJECT_ROOT ?= $(shell pwd)
LOCALBIN ?= $(PROJECT_ROOT)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

GOLANGCI_LINT_VERSION ?= v2.11.4
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

CONTROLLER_TOOLS_VERSION ?= v0.20.1
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

## --------------------------------------
## Tool Dependencies
## --------------------------------------

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(GOLANGCI_LINT) && $(GOLANGCI_LINT) --version | grep -q $(GOLANGCI_LINT_VERSION) || \
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(CONTROLLER_GEN) && $(CONTROLLER_GEN) --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

## --------------------------------------
## Code Generation
## --------------------------------------

.PHONY: manifests
manifests: controller-gen ## Generate RBAC ClusterRole from kubebuilder markers into the Helm chart.
	$(CONTROLLER_GEN) rbac:roleName=gpu-node-mocker paths="./pkg/..." output:rbac:artifacts:config=charts/gpu-node-mocker/templates
	@mv charts/gpu-node-mocker/templates/role.yaml charts/gpu-node-mocker/templates/clusterrole-auto-generated.yaml
	@echo "Generated charts/gpu-node-mocker/templates/clusterrole-auto-generated.yaml"

.PHONY: verify-manifests
verify-manifests: manifests ## Verify generated manifests are up to date.
	@echo "verifying manifests"
	@if [ -n "$$(git status --porcelain charts/)" ]; then \
		echo "Error: manifests are not up-to-date. Run 'make manifests' and commit the changes."; \
		git diff charts/; \
		exit 1; \
	fi

## --------------------------------------
## CI Targets
## --------------------------------------

.PHONY: verify-mod
verify-mod: ## Verify go.mod and go.sum are tidy.
	@echo "verifying go.mod and go.sum"
	go mod tidy
	@if [ -n "$$(git status --porcelain go.mod go.sum)" ]; then \
		echo "Error: go.mod/go.sum is not up-to-date. please run 'go mod tidy' and commit the changes."; \
		git diff go.mod go.sum; \
		exit 1; \
	fi

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint.
	$(GOLANGCI_LINT) run -v

.PHONY: verify-boilerplate
verify-boilerplate: ## Verify all Go files have the required license header.
	@bash hack/verify-boilerplate.sh

## --------------------------------------
## Build
## --------------------------------------

OUTPUT_DIR := $(PROJECT_ROOT)/_output
REGISTRY ?= ghcr.io/kaito-project
IMG_NAME ?= gpu-node-mocker
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMG_TAG ?= $(VERSION)
IMG ?= $(REGISTRY)/$(IMG_NAME):$(IMG_TAG)

ARCH ?= amd64

.PHONY: build
build: ## Build the gpu-node-mocker binary.
	@mkdir -p $(OUTPUT_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o $(OUTPUT_DIR)/gpu-node-mocker ./cmd/gpu-node-mocker

.PHONY: test
test: ## Run unit tests.
	go test -v -race -count=1 ./pkg/... ./cmd/...

CONTAINER_TOOL ?= $(shell command -v docker 2>/dev/null || command -v podman 2>/dev/null || echo docker)

.PHONY: docker-build
docker-build: ## Build docker image for the target ARCH.
	$(CONTAINER_TOOL) build --platform linux/$(ARCH) -f docker/Dockerfile -t $(IMG) .

# Multi-arch buildx target used by the release workflow. Set
#   OUTPUT_TYPE=type=registry  to push directly to $(IMG), or
#   OUTPUT_TYPE=type=docker    to load into the local daemon (single arch only).
PLATFORMS ?= linux/amd64,linux/arm64
OUTPUT_TYPE ?= type=docker

.PHONY: docker-buildx
docker-buildx: ## Multi-arch build (and optionally push) the gpu-node-mocker image.
	$(CONTAINER_TOOL) buildx inspect production-stack-builder >/dev/null 2>&1 || \
		$(CONTAINER_TOOL) buildx create --name production-stack-builder --use
	$(CONTAINER_TOOL) buildx build \
		--platform $(PLATFORMS) \
		--output=$(OUTPUT_TYPE) \
		-f docker/Dockerfile \
		-t $(IMG) .

## --------------------------------------
## E2E Tests
## --------------------------------------

E2E_LABEL ?=
E2E_PARALLEL ?= 2

.PHONY: test-e2e
test-e2e: ## Run e2e tests against a live cluster (requires KUBECONFIG).
	@echo "Running e2e tests (parallel=$(E2E_PARALLEL))..."
	go run github.com/onsi/ginkgo/v2/ginkgo \
		--procs=$(E2E_PARALLEL) \
		--timeout=30m \
		-v \
		$(if $(E2E_LABEL),--label-filter="$(E2E_LABEL)",) \
		./test/e2e/...
## --------------------------------------
## E2E Targets
##
## Component versions are centralized in versions.env (repo root).
## Override any version via environment variables, e.g.:
##   ISTIO_VERSION=1.30.0 BBR_VERSION=v1.4.0 make e2e-install
##
## The E2E_PROVIDER master switch (default: upstream) selects how
## infrastructure components are sourced:
##   upstream → install everything via Helm/upstream manifests
##   azure    → enable the AKS managed KEDA add-on at cluster create
##              time and skip the standalone Helm KEDA install
## --------------------------------------

.PHONY: e2e
e2e: ## Run full E2E cycle: setup cluster, install, validate, test, teardown.
	hack/e2e/scripts/run-e2e-local.sh all

.PHONY: e2e-setup
e2e-setup: ## Create AKS cluster (requires RG+ACR; run e2e-prepare-image first).
	hack/e2e/scripts/run-e2e-local.sh setup

.PHONY: e2e-prepare-image
e2e-prepare-image: ## Create RG+ACR and build/push the gpu-node-mocker image to ACR.
	hack/e2e/scripts/prepare-image.sh

.PHONY: e2e-push-image
e2e-push-image: ## Tag and push image to ACR. Sets SHADOW_CONTROLLER_IMAGE.
	az acr login --name "$${ACR_NAME}" >&2; \
	IMAGE_TAG="latest-$$(head -c 8 /dev/urandom | xxd -p)"; \
	IMAGE="$${ACR_NAME}.azurecr.io/gpu-node-mocker:$${IMAGE_TAG}"; \
	$(CONTAINER_TOOL) tag $(IMG) "$${IMAGE}" >&2; \
	$(CONTAINER_TOOL) push "$${IMAGE}" >&2; \
	echo "image=$${IMAGE}"

.PHONY: e2e-push-image-local
e2e-push-image-local: ## Tag and push locally-built image to ACR (no hash, uses IMG).
	az acr login --name "$${ACR_NAME}" >&2; \
	IMAGE="$${ACR_NAME}.azurecr.io/gpu-node-mocker:latest"; \
	$(CONTAINER_TOOL) tag $(IMG) "$${IMAGE}" >&2; \
	$(CONTAINER_TOOL) push "$${IMAGE}" >&2; \
	echo "image=$${IMAGE}"

.PHONY: e2e-install
e2e-install: ## Install all E2E components onto the cluster.
	hack/e2e/scripts/run-e2e-local.sh install

.PHONY: e2e-validate
e2e-validate: ## Validate all E2E components are healthy.
	hack/e2e/scripts/run-e2e-local.sh validate

.PHONY: e2e-dump
e2e-dump: ## Dump cluster state for debugging.
	hack/e2e/scripts/dump-cluster-state.sh

USER_ID ?= $(shell whoami)
# Honor pre-set CLUSTER_NAME / RESOURCE_GROUP env vars (e.g. from CI) so they
# are not silently overwritten with the local-developer default of kaito-$(USER_ID).
E2E_CLUSTER_NAME ?= $(if $(CLUSTER_NAME),$(CLUSTER_NAME),kaito-$(USER_ID))
E2E_RESOURCE_GROUP ?= $(if $(RESOURCE_GROUP),$(RESOURCE_GROUP),kaito-$(USER_ID))

.PHONY: e2e-teardown
e2e-teardown: ## Tear down the E2E cluster.
	CLUSTER_NAME=$(E2E_CLUSTER_NAME) RESOURCE_GROUP=$(E2E_RESOURCE_GROUP) hack/e2e/scripts/run-e2e-local.sh teardown

.PHONY: e2e-up
e2e-up: ## One command to set up full local E2E env (cluster, build, push, install, validate).
	@set -e; \
	export CLUSTER_NAME=$(E2E_CLUSTER_NAME) RESOURCE_GROUP=$(E2E_RESOURCE_GROUP); \
	IMAGE_LINE=$$(hack/e2e/scripts/prepare-image.sh | grep '^image='); \
	export SHADOW_CONTROLLER_IMAGE=$${IMAGE_LINE#image=}; \
	echo "Using SHADOW_CONTROLLER_IMAGE=$${SHADOW_CONTROLLER_IMAGE}"; \
	hack/e2e/scripts/run-e2e-local.sh setup; \
	hack/e2e/scripts/run-e2e-local.sh install; \
	hack/e2e/scripts/run-e2e-local.sh validate; \
	echo ""; \
	echo "=== E2E environment is ready ==="; \
	echo "  Cluster: $(E2E_CLUSTER_NAME)"; \
	echo "  Resource Group: $(E2E_RESOURCE_GROUP)"; \
	echo "  Image:   $${SHADOW_CONTROLLER_IMAGE}"; \
	echo "Run tests with: make test-e2e"; \
	echo "Tear down with: CLUSTER_NAME=$(E2E_CLUSTER_NAME) RESOURCE_GROUP=$(E2E_RESOURCE_GROUP) make e2e-teardown"

## --------------------------------------
## Helm
## --------------------------------------

##@ Helm

.PHONY: helm-package-and-push
helm-package-and-push: ## Package and Push Helm Chart.
	sed -Ei 's/^(appVersion|version):.*$$/\1: $(CHART_VERSION)/g' ./charts/$(CHART_NAME)/Chart.yaml
	cd ./charts/$(CHART_NAME) && helm package .
	helm push ./charts/$(CHART_NAME)/$(CHART_NAME)-$(CHART_VERSION).tgz oci://$(CHART_REGISTRY)
