VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GO      ?= go
ARCH    ?= $(shell $(GO) env GOARCH)

.DEFAULT_GOAL := build

.PHONY: build
build: ## thaw for this machine, and thaw-guest for Linux, into ./bin
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/thaw ./cmd/thaw
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) $(GO) build -trimpath -ldflags '-s -w' -o bin/thaw-guest ./cmd/thaw-guest

.PHONY: install
install: build ## install both into $GOPATH/bin
	install -m 0755 bin/thaw bin/thaw-guest $(shell $(GO) env GOPATH)/bin/

.PHONY: test
test: ## unit tests; the uffd and nbd tests need root on Linux and skip otherwise
	$(GO) test -race ./...

.PHONY: vet
vet:
	$(GO) vet ./...
	GOOS=linux $(GO) vet ./...

.PHONY: web
web: ## build the site into web/out
	cd web && pnpm install --frozen-lockfile && pnpm build

.PHONY: help
help:
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'
