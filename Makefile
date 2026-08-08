# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Stamped into the binary so a running pod reports what it is. A release build
# passes the tag explicitly; a local build describes the checkout.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -s -w -X main.version=$(VERSION)

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# The controller envtest suite drives real failover and scaling timings and runs
# past go test's 10m default, which would fail the run on the deadline alone.
UNIT_TIMEOUT ?= 30m

# Without -coverpkg every package only counts the lines its own tests execute,
# so code exercised through another package's tests reads as zero.
COVERPROFILE ?= cover.out
COVERPKG ?= ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v /e2e) -timeout $(UNIT_TIMEOUT) -coverprofile $(COVERPROFILE) -coverpkg $(COVERPKG)

.PHONY: cover-report
cover-report: ## Print total statement coverage from the last test run.
	@test -f "$(COVERPROFILE)" || { echo "$(COVERPROFILE) not found; run 'make test' first."; exit 1; }
	@go tool cover -func="$(COVERPROFILE)" | tail -n 1

# The e2e suite builds the operator image, side-loads it into kind and installs
# the chart from charts/redguard, which is the path a user follows.
#
# Every run gets a cluster name of its own, so two suites running at once cannot
# share a cluster or delete each other's. Passing KIND_CLUSTER reuses a named
# cluster; cleanup then refuses to delete it, because this run did not make it.
# The name is fixed per make invocation, so run the suite as `make test-e2e`:
# `make setup-test-e2e` on its own creates a cluster no later run will reuse.
ifndef E2E_RUN_ID
E2E_RUN_ID := $(shell od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
endif
KIND_CLUSTER ?= redguard-e2e-$(E2E_RUN_ID)

# The kubeconfig the developer's own kubectl reads. The e2e path must leave it
# byte for byte alone: an earlier revision switched its current context as a
# side effect, which aimed a mid-suite `helm uninstall` at a live cluster.
E2E_SHARED_KUBECONFIG := $(if $(KUBECONFIG),$(firstword $(subst :, ,$(KUBECONFIG))),$(HOME)/.kube/config)

# LOCALBIN is defined further down, so these stay recursively expanded.
E2E_RUN_DIR = $(LOCALBIN)/e2e/$(KIND_CLUSTER)
# kind writes the shared kubeconfig and switches the current context unless it
# is handed a file of its own. Nothing in the e2e path may touch ~/.kube/config.
E2E_KUBECONFIG = $(E2E_RUN_DIR)/kubeconfig
# Written only when this run created the cluster. cleanup-test-e2e refuses
# without it, so a cluster belonging to someone else survives a stray cleanup.
E2E_OWNED = $(E2E_RUN_DIR)/created-by-this-run
# The suite names its own kubeconfig on every kubectl and helm command line.
# This is what the environment holds meanwhile: a context that resolves to
# nothing, so a call that escaped the pin fails instead of reaching a cluster.
E2E_DECOY_KUBECONFIG = $(E2E_RUN_DIR)/decoy-kubeconfig
# Digest of the shared kubeconfig taken before the cluster is created and
# compared after, so a target that writes it is caught in the run that did it.
E2E_SHARED_DIGEST = $(E2E_RUN_DIR)/shared-kubeconfig.cksum

# Booting a Redis cluster and forcing a failover takes longer than the 10m
# default go test deadline, which would kill the run mid-suite.
E2E_TIMEOUT ?= 45m

# Node image the e2e cluster runs, e.g. kindest/node:v1.34.8. Empty keeps the
# default of the installed kind; CI sets it to sweep Kubernetes versions.
KIND_NODE_IMAGE ?=

.PHONY: setup-test-e2e
setup-test-e2e: ## Create a Kind cluster for the e2e suite, reachable only through its own kubeconfig
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "kind is not installed. Install it before running the e2e suite."; \
		exit 1; \
	}
	@$(CONTAINER_TOOL) info >/dev/null 2>&1 || { \
		echo "$(CONTAINER_TOOL) is not running; the suite has to build the operator image."; \
		exit 1; \
	}
	@mkdir -p "$(E2E_RUN_DIR)"
	@if [ -f "$(E2E_SHARED_KUBECONFIG)" ]; then \
		cksum < "$(E2E_SHARED_KUBECONFIG)" > "$(E2E_SHARED_DIGEST)"; \
	else \
		echo absent > "$(E2E_SHARED_DIGEST)"; \
	fi
	@if $(KIND) get clusters 2>/dev/null | grep -qx "$(KIND_CLUSTER)"; then \
		echo "Kind cluster '$(KIND_CLUSTER)' already exists and was not created by this run."; \
		rm -f "$(E2E_OWNED)"; \
	else \
		echo "Creating Kind cluster '$(KIND_CLUSTER)'..."; \
		$(KIND) create cluster --name "$(KIND_CLUSTER)" --kubeconfig "$(E2E_KUBECONFIG)" \
			$(if $(KIND_NODE_IMAGE),--image "$(KIND_NODE_IMAGE)",); \
		echo "$(KIND_CLUSTER)" > "$(E2E_OWNED)"; \
	fi
	@$(KIND) get kubeconfig --name "$(KIND_CLUSTER)" > "$(E2E_KUBECONFIG)"
	@chmod 600 "$(E2E_KUBECONFIG)"
	@printf '%s\n' 'apiVersion: v1' 'kind: Config' \
		'current-context: redguard-e2e-decoy-never-use-this' \
		'clusters: []' 'contexts: []' 'users: []' > "$(E2E_DECOY_KUBECONFIG)"
	@KUBECONFIG="$(E2E_KUBECONFIG)" $(KUBECTL) --context "kind-$(KIND_CLUSTER)" \
		wait --for=condition=Ready node --all --timeout=300s
	@if [ -f "$(E2E_SHARED_KUBECONFIG)" ]; then \
		after=$$(cksum < "$(E2E_SHARED_KUBECONFIG)"); \
	else \
		after=absent; \
	fi; \
	if [ "$$after" != "$$(cat "$(E2E_SHARED_DIGEST)")" ]; then \
		echo "setup-test-e2e wrote $(E2E_SHARED_KUBECONFIG); the e2e path must never touch the shared kubeconfig."; \
		exit 1; \
	fi

.PHONY: test-e2e
test-e2e: setup-test-e2e manifests generate fmt vet ## Run the e2e tests against a Kind cluster of this run's own
	@status=0; \
	KUBECONFIG="$(E2E_DECOY_KUBECONFIG)" KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) \
	CONTAINER_TOOL=$(CONTAINER_TOOL) \
		go test -tags=e2e ./test/e2e/ -v -timeout $(E2E_TIMEOUT) -ginkgo.v || status=$$?; \
	if [ -f "$(E2E_OWNED)" ]; then \
		$(MAKE) cleanup-test-e2e KIND_CLUSTER=$(KIND_CLUSTER) || { [ $$status -ne 0 ] || status=1; }; \
	else \
		echo "Keeping kind cluster '$(KIND_CLUSTER)': this run did not create it."; \
	fi; \
	exit $$status

.PHONY: cleanup-test-e2e
cleanup-test-e2e: ## Tear down the Kind cluster this run created, and only that one
	@if [ ! -f "$(E2E_OWNED)" ]; then \
		echo "Refusing to delete kind cluster '$(KIND_CLUSTER)': this run did not create it."; \
		exit 1; \
	fi
	@owner=$$(cat "$(E2E_OWNED)"); \
	if [ "$$owner" != "$(KIND_CLUSTER)" ]; then \
		echo "Refusing to delete kind cluster '$(KIND_CLUSTER)': the marker names '$$owner'."; \
		exit 1; \
	fi
	@before=$$(if [ -f "$(E2E_SHARED_KUBECONFIG)" ]; then cksum < "$(E2E_SHARED_KUBECONFIG)"; else echo absent; fi); \
	$(KIND) delete cluster --name "$(KIND_CLUSTER)" --kubeconfig "$(E2E_KUBECONFIG)" || exit $$?; \
	after=$$(if [ -f "$(E2E_SHARED_KUBECONFIG)" ]; then cksum < "$(E2E_SHARED_KUBECONFIG)"; else echo absent; fi); \
	if [ "$$before" != "$$after" ]; then \
		echo "cleanup-test-e2e wrote $(E2E_SHARED_KUBECONFIG); teardown must never touch the shared kubeconfig."; \
		exit 1; \
	fi
	@rm -rf "$(E2E_RUN_DIR)"

# Chart files written by hack/sync-chart.sh. Anything else under charts/ is
# hand-maintained and is not part of the drift gate.
CHART_DIR := charts/redguard
CHART_GENERATED := crds \
	templates/role.yaml templates/rolebinding.yaml \
	templates/leader-election-role.yaml templates/leader-election-rolebinding.yaml \
	templates/metrics-auth-role.yaml templates/metrics-auth-rolebinding.yaml

.PHONY: sync-chart
sync-chart: manifests ## Regenerate chart CRDs and RBAC from config/.
	./hack/sync-chart.sh

.PHONY: verify-chart
verify-chart: manifests ## Fail if the chart is out of sync with config/ or drops a required runtime flag.
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT; \
	./hack/sync-chart.sh "$$tmp" >/dev/null; \
	rc=0; \
	for f in $(CHART_GENERATED); do \
		diff -ru "$(CHART_DIR)/$$f" "$$tmp/$$f" || rc=1; \
	done; \
	if [ $$rc -ne 0 ]; then \
		echo "ERROR: $(CHART_DIR) is out of sync with config/. Run 'make sync-chart' and commit."; \
		exit 1; \
	fi
	helm lint $(CHART_DIR)
	helm template redguard $(CHART_DIR) >/dev/null
	./hack/verify-chart-render.sh $(CHART_DIR)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -ldflags "$(LDFLAGS)" -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name redguard-builder
	$(CONTAINER_TOOL) buildx use redguard-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --build-arg VERSION=$(VERSION) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm redguard-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" apply -f -; else echo "No CRDs to install; skipping."; fi

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	@out="$$( "$(KUSTOMIZE)" build config/crd 2>/dev/null || true )"; \
	if [ -n "$$out" ]; then echo "$$out" | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -; else echo "No CRDs to delete; skipping."; fi

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && "$(KUSTOMIZE)" edit set image controller=${IMG}
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.7.1
CONTROLLER_TOOLS_VERSION ?= v0.19.0

#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.5.0
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

##@ Local Testing

.PHONY: helm-install-local
helm-install-local: ## Install operator with Helm (local)
	helm upgrade --install redguard ./$(CHART_DIR) \
		--values ./$(CHART_DIR)/values-local.yaml \
		--namespace redguard-system \
		--create-namespace \
		--wait

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall operator Helm release
	helm uninstall redguard -n redguard-system
