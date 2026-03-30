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

.PHONY: docker-build
docker-build: ## Build docker image.
	docker build -f docker/Dockerfile -t $(IMG) .
