IMG ?= llm-authz:latest
CHART_DIR ?= chart/llm-authz
RELEASE_NAME ?= llm-authz
NAMESPACE ?= llm-gateway

.PHONY: all build test lint fmt vet generate manifests docker-build docker-push install uninstall deploy helm-install helm-upgrade helm-uninstall helm-template

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

docker-build:
	docker build -t $(IMG) .

docker-push:
	docker push $(IMG)

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
