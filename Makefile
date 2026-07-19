# mamori - multi-module monorepo Makefile.
# Each module is built/tested independently with the workspace disabled, matching CI.

# All Go modules in the repo (dirs containing a go.mod, excluding testdata and site).
MODULES := . \
	providers/aws providers/gcp providers/azure providers/vault providers/k8s \
	providers/consul providers/doppler providers/onepassword providers/sops \
	x/otel tools/reconcilevet

GO ?= go
export GOWORK = off

.PHONY: all test race lint vet fmt tidy build vet-analyzer work-sync site-dev site-build clean help

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

all: tidy build test lint ## Tidy, build, test, and lint every module

build: ## go build every module
	@for m in $(MODULES); do echo "==> build $$m"; (cd $$m && $(GO) build ./...) || exit 1; done

test: ## go test every module
	@for m in $(MODULES); do echo "==> test $$m"; (cd $$m && $(GO) test ./...) || exit 1; done

race: ## go test -race every module
	@for m in $(MODULES); do echo "==> test -race $$m"; (cd $$m && $(GO) test -race ./...) || exit 1; done

vet: ## go vet every module
	@for m in $(MODULES); do echo "==> vet $$m"; (cd $$m && $(GO) vet ./...) || exit 1; done

lint: ## golangci-lint every module (requires golangci-lint)
	@for m in $(MODULES); do echo "==> lint $$m"; (cd $$m && golangci-lint run --timeout=5m) || exit 1; done

fmt: ## gofmt -w the whole tree
	@gofmt -w $$(find . -name '*.go' -not -path './**/testdata/*')

tidy: ## go mod tidy every module
	@for m in $(MODULES); do echo "==> tidy $$m"; (cd $$m && $(GO) mod tidy) || exit 1; done

vet-analyzer: ## Build reconcilevet and run it over core + examples
	@cd tools/reconcilevet && $(GO) build -o /tmp/reconcilevet ./cmd/reconcilevet
	@$(GO) vet -vettool=/tmp/reconcilevet ./... ./examples/...

work-sync: ## Regenerate the go.work workspace (then re-tidy modules)
	@GOWORK= $(GO) work sync
	@$(MAKE) tidy

site-dev: ## Run the docs site dev server
	@cd site && npm install && npm run dev

site-build: ## Build the docs site
	@cd site && npm install && npm run build

clean: ## Remove build artifacts
	@rm -rf dist site/dist site/.astro /tmp/reconcilevet
