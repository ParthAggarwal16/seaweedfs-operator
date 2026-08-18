# seaweedfs-operator
#
# Run `make help` for the list of targets.

IMG            ?= ghcr.io/openeverest/seaweedfs-operator:dev
ENVTEST_K8S_VERSION ?= 1.31.0
KIND_CLUSTER   ?= seaweedfs-operator
NAMESPACE      ?= seaweedfs-system

LOCALBIN       := $(shell pwd)/bin
CONTROLLER_GEN := $(LOCALBIN)/controller-gen
SETUP_ENVTEST  := $(LOCALBIN)/setup-envtest
KUSTOMIZE      := $(LOCALBIN)/kustomize

CONTROLLER_TOOLS_VERSION ?= v0.17.3
KUSTOMIZE_VERSION        ?= v5.5.0

# Pinning the toolchain avoids Go silently downloading a different one.
GO ?= go

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Regenerate CRDs and RBAC from the kubebuilder markers.
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=false \
		rbac:roleName=seaweedfs-operator-manager \
		paths=./... \
		output:crd:artifacts:config=config/crd \
		output:rbac:artifacts:config=config/rbac
	cp config/crd/*.yaml charts/seaweedfs-operator/crds/

.PHONY: generate
generate: controller-gen ## Regenerate DeepCopy methods.
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

.PHONY: fmt
fmt: ## Format the code.
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet.
	$(GO) vet ./...

.PHONY: lint
lint: fmt vet ## Format and vet.

##@ Testing

.PHONY: test
test: manifests generate fmt vet envtest ## Run unit and envtest suites.
	KUBEBUILDER_ASSETS="$(shell $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		$(GO) test ./... -coverprofile cover.out

.PHONY: test-unit
test-unit: ## Run only the tests that need no control plane.
	$(GO) test ./internal/resources/... ./internal/seaweed/...

.PHONY: test-e2e
test-e2e: ## Run the end-to-end suite against a real kind cluster with real SeaweedFS.
	./test/e2e/run.sh

.PHONY: cover
cover: test ## Show coverage in a browser.
	$(GO) tool cover -html=cover.out

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the manager binary.
	$(GO) build -o bin/manager ./cmd

.PHONY: run
run: manifests generate ## Run the operator against the current kubecontext.
	$(GO) run ./cmd --zap-devel

.PHONY: docker-build
docker-build: ## Build the operator image.
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the operator image.
	docker push $(IMG)

##@ Deployment

.PHONY: install
install: manifests ## Install the CRDs into the current cluster.
	kubectl apply -f config/crd

.PHONY: uninstall
uninstall: ## Remove the CRDs from the current cluster.
	kubectl delete --ignore-not-found -f config/crd

.PHONY: deploy
deploy: manifests ## Deploy the operator into the current cluster.
	kubectl apply -f config/crd
	kubectl apply -f config/manager/namespace.yaml
	kubectl apply -f config/rbac
	sed 's|IMAGE_PLACEHOLDER|$(IMG)|g' config/manager/manager.yaml | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Remove the operator from the current cluster.
	kubectl delete --ignore-not-found -f config/manager/manager.yaml
	kubectl delete --ignore-not-found -f config/rbac
	kubectl delete --ignore-not-found -f config/manager/namespace.yaml

##@ Kind

.PHONY: kind-up
kind-up: ## Create a local kind cluster.
	kind create cluster --name $(KIND_CLUSTER) --config test/e2e/kind-config.yaml || true
	kubectl cluster-info --context kind-$(KIND_CLUSTER)

.PHONY: kind-down
kind-down: ## Delete the local kind cluster.
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: kind-load
kind-load: docker-build ## Build the image and load it into kind.
	kind load docker-image $(IMG) --name $(KIND_CLUSTER)

.PHONY: kind-deploy
kind-deploy: kind-load deploy ## Build, load and deploy into kind in one step.

##@ Tools

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(CONTROLLER_GEN) || \
		GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(SETUP_ENVTEST)
$(SETUP_ENVTEST): $(LOCALBIN)
	test -s $(SETUP_ENVTEST) || \
		GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.21
	$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path > /dev/null

.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): $(LOCALBIN)
	test -s $(KUSTOMIZE) || \
		GOBIN=$(LOCALBIN) $(GO) install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)
