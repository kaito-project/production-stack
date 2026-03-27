# Makefile for production-stack

PROJECT_ROOT ?= $(shell pwd)
LOCALBIN ?= $(PROJECT_ROOT)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

GOLANGCI_LINT_VERSION ?= v2.11.4
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

## --------------------------------------
## Tool Dependencies
## --------------------------------------

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT)
$(GOLANGCI_LINT): $(LOCALBIN)
	test -s $(GOLANGCI_LINT) && $(GOLANGCI_LINT) --version | grep -q $(GOLANGCI_LINT_VERSION) || \
	GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

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
