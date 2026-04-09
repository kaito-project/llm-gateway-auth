# Image URL to use all building/pushing image targets
IMG ?= llm-gateway-auth:latest
OPERATOR_IMG        ?= apikey-operator:latest
AUTHZ_IMG           ?= apikey-istio-authz-ext:latest
OPERATOR_DOCKERFILE ?= Dockerfile.apikey-operator
AUTHZ_DOCKERFILE    ?= Dockerfile.apikey-istio-authz-ext

# Helm configuration
CHART_DIR ?= chart/llm-gateway-apikey
RELEASE_NAME ?= llm-gateway-apikey
NAMESPACE ?= llm-gateway

# Container runtime: auto-detect docker or podman. Override with:
#   make image-build CONTAINER_RUNTIME=podman
CONTAINER_RUNTIME ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)

.PHONY: all build test lint fmt vet generate manifests image-build image-push install uninstall deploy helm-install helm-upgrade helm-uninstall helm-template kind-create kind-delete setup-test-env teardown-test-env kind-load image-build-operator image-build-authz image-build-all image-push-operator image-push-authz kind-load-operator kind-load-authz kind-load-all

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

## --------------------------------------
## Tooling Binaries
## --------------------------------------

##@ Tooling Binaries

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN): ## Create folder for installing local binaries.
	mkdir -p $(LOCALBIN)

.PHONY: localbin
localbin: $(LOCALBIN) ## Create folder for installing local binaries.

## Tool Binaries
KUBECTL ?= kubectl
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.20.1

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary. If wrong version is installed, it will be overwritten.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen && $(LOCALBIN)/controller-gen --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

## --------------------------------------
## Development
## --------------------------------------

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole, and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object paths="./api/..." output:object:dir=api/v1alpha1

## --------------------------------------
## Linting
## --------------------------------------

##@ Linting

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint against code.
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

## --------------------------------------
## Tests
## --------------------------------------

##@ Tests

.PHONY: test
test: ## Run unit tests.
	go test ./... -coverprofile=cover.out -count=1

.PHONY: e2e-test
e2e-test: ## Run e2e tests against the kind cluster (requires: make kind-create).
	USE_EXISTING_CLUSTER=true go test -tags e2e ./test/e2e/... -v -count=1

## --------------------------------------
## Build
## --------------------------------------

##@ Build

.PHONY: all
all: generate fmt vet build ## Run generate, fmt, vet, and build.

.PHONY: build
build: ## Build manager binary.
	go build -ldflags="-s -w" -o bin/apikey-operator ./cmd/apikey-operator/
	go build -ldflags="-s -w" -o bin/apikey-istio-authz-ext ./cmd/apikey-istio-authz-ext/

.PHONY: tidy
tidy: ## Run go mod tidy.
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf bin/ cover.out

## --------------------------------------
## Docker
## --------------------------------------

##@ Docker

## image-build: Build the container image (docker or podman)
image-build:
	$(CONTAINER_RUNTIME) build -t $(IMG) .

## image-build-operator: Build the apikey-operator image
image-build-operator:
	$(CONTAINER_RUNTIME) build -f $(OPERATOR_DOCKERFILE) -t $(OPERATOR_IMG) .

## image-build-authz: Build the apikey-istio-authz-ext image
image-build-authz:
	$(CONTAINER_RUNTIME) build -f $(AUTHZ_DOCKERFILE) -t $(AUTHZ_IMG) .

## image-build-all: Build all three images (azure + operator + authz)
image-build-all: image-build image-build-operator image-build-authz

## image-push: Push the container image
image-push:
	$(CONTAINER_RUNTIME) push $(IMG)

## image-push-operator: Push the apikey-operator image
image-push-operator:
	$(CONTAINER_RUNTIME) push $(OPERATOR_IMG)

## image-push-authz: Push the apikey-istio-authz-ext image
image-push-authz:
	$(CONTAINER_RUNTIME) push $(AUTHZ_IMG)

## Backwards-compatible aliases
docker-build: image-build
docker-push: image-push

## --------------------------------------
## Deployment
## --------------------------------------

##@ Deployment

.PHONY: install
install: ## Install CRDs into the cluster.
	kubectl apply -f config/crd/

.PHONY: uninstall
uninstall: ## Uninstall CRDs from the cluster.
	kubectl delete -f config/crd/

.PHONY: deploy
deploy: install ## Deploy controller to the cluster.
	kubectl apply -f config/rbac/
	kubectl apply -f config/deployment/
	kubectl apply -f config/istio/

## --------------------------------------
## Helm
## --------------------------------------

##@ Helm

.PHONY: helm-install
helm-install: ## Install Helm chart.
	helm install $(RELEASE_NAME) $(CHART_DIR) --namespace $(NAMESPACE) --create-namespace

.PHONY: helm-upgrade
helm-upgrade: ## Upgrade Helm release.
	helm upgrade $(RELEASE_NAME) $(CHART_DIR) --namespace $(NAMESPACE)

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall Helm release.
	helm uninstall $(RELEASE_NAME) --namespace $(NAMESPACE)

.PHONY: helm-template
helm-template: ## Render Helm chart templates locally.
	helm template $(RELEASE_NAME) $(CHART_DIR) --namespace $(NAMESPACE)

## ─── Test Environment ────────────────────────────────────────────────────────
## Override at the command line: make setup-test-env KIND_CLUSTER_NAME=my-cluster

# Load pinned versions from the single source of truth.
include versions.env

KIND_CLUSTER_NAME  ?= llm-gateway-test

## kind-create: Create the kind cluster (no Istio)
kind-create:
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) \
	CONTAINER_RUNTIME=$(CONTAINER_RUNTIME) \
	SKIP_ISTIO=true \
	hack/setup-test-env.sh

## kind-delete: Tear down the kind cluster
kind-delete:
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) \
	CONTAINER_RUNTIME=$(CONTAINER_RUNTIME) \
	hack/teardown-test-env.sh

## setup-test-env: Create kind cluster + install Istio
setup-test-env:
	KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) \
	NAMESPACE=$(NAMESPACE) \
	CONTAINER_RUNTIME=$(CONTAINER_RUNTIME) \
	hack/setup-test-env.sh

## teardown-test-env: Tear down the kind cluster
teardown-test-env: kind-delete

## kind-load: Build the container image and load it into the kind cluster
kind-load: image-build
ifeq ($(CONTAINER_RUNTIME),podman)
	podman save $(IMG) -o /tmp/kind-image.tar
	kind load image-archive /tmp/kind-image.tar --name $(KIND_CLUSTER_NAME)
	rm -f /tmp/kind-image.tar
else
	kind load docker-image $(IMG) --name $(KIND_CLUSTER_NAME)
endif

## kind-load-operator: Build and load apikey-operator image into kind
kind-load-operator: image-build-operator
ifeq ($(CONTAINER_RUNTIME),podman)
	podman save $(OPERATOR_IMG) -o /tmp/kind-operator.tar
	kind load image-archive /tmp/kind-operator.tar --name $(KIND_CLUSTER_NAME)
	rm -f /tmp/kind-operator.tar
else
	kind load docker-image $(OPERATOR_IMG) --name $(KIND_CLUSTER_NAME)
endif

## kind-load-authz: Build and load apikey-istio-authz-ext image into kind
kind-load-authz: image-build-authz
ifeq ($(CONTAINER_RUNTIME),podman)
	podman save $(AUTHZ_IMG) -o /tmp/kind-authz.tar
	kind load image-archive /tmp/kind-authz.tar --name $(KIND_CLUSTER_NAME)
	rm -f /tmp/kind-authz.tar
else
	kind load docker-image $(AUTHZ_IMG) --name $(KIND_CLUSTER_NAME)
endif

## kind-load-all: Build and load all images into kind
kind-load-all: kind-load kind-load-operator kind-load-authz
