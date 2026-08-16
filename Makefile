.PHONY: build test test-verbose fmt vet lint demo docker-build kind-demo kind-down clean

GO ?= go
BIN_DIR := bin

build: ## Build operator and mock-infoblox binaries
	$(GO) build -o $(BIN_DIR)/operator ./cmd/main.go
	$(GO) build -o $(BIN_DIR)/mock-infoblox ./cmd/mock-infoblox

test: ## Run unit tests (internal/infoblox is dependency-free and always runnable)
	$(GO) test ./... -race -count=1

test-verbose:
	$(GO) test ./... -race -count=1 -v

fmt: ## Format all Go source
	gofmt -w .

vet: ## Static analysis
	$(GO) vet ./...

lint: fmt vet ## Format + vet as a cheap pre-commit gate

demo: ## Run the no-cluster curl-based lifecycle demo against mock-infoblox
	./demo/demo.sh

kind-demo: ## Full in-cluster demo: build images, kind cluster, deploy, apply sample claim
	./scripts/kind-demo.sh

kind-down: ## Tear down the demo kind cluster
	kind delete cluster --name infoblox-ipam-demo

docker-build: ## Build both container images
	docker build -t infoblox-ipam-operator:dev --target operator .
	docker build -t mock-infoblox:dev --target mock-infoblox .

clean:
	rm -rf $(BIN_DIR)
