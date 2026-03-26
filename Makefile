IMG ?= llm-authz:latest
CHART_DIR ?= chart/llm-authz
RELEASE_NAME ?= llm-authz
NAMESPACE ?= llm-gateway

# Container runtime: auto-detect docker or podman. Override with:
#   make image-build CONTAINER_RUNTIME=podman
CONTAINER_RUNTIME ?= $(shell command -v docker >/dev/null 2>&1 && echo docker || echo podman)

.PHONY: all build test lint fmt vet generate manifests image-build image-push install uninstall deploy helm-install helm-upgrade helm-uninstall helm-template kind-create kind-delete setup-test-env teardown-test-env kind-load

all: generate fmt vet build

build:
	go build -ldflags="-s -w" -o bin/llm-authz ./cmd/

test:
	go test ./... -coverprofile=cover.out -count=1

lint:
	golangci-lint run ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

generate:
	controller-gen object paths="./api/..." output:object:dir=api/v1alpha1

manifests:
	controller-gen crd paths="./api/..." output:crd:dir=config/crd

## image-build: Build the container image (docker or podman)
image-build:
	$(CONTAINER_RUNTIME) build -t $(IMG) .

## image-push: Push the container image
image-push:
	$(CONTAINER_RUNTIME) push $(IMG)

## Backwards-compatible aliases
docker-build: image-build
docker-push: image-push

install:
	kubectl apply -f config/crd/

uninstall:
	kubectl delete -f config/crd/

deploy: install
	kubectl apply -f config/rbac/
	kubectl apply -f config/deployment/
	kubectl apply -f config/istio/

clean:
	rm -rf bin/ cover.out

tidy:
	go mod tidy

helm-install:
	helm install $(RELEASE_NAME) $(CHART_DIR) --namespace $(NAMESPACE) --create-namespace

helm-upgrade:
	helm upgrade $(RELEASE_NAME) $(CHART_DIR) --namespace $(NAMESPACE)

helm-uninstall:
	helm uninstall $(RELEASE_NAME) --namespace $(NAMESPACE)

helm-template:
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
