# Verbosity
V ?= 0
Q := $(if $(filter 1,$(V)),,@)

GO ?= go
GOFMT ?= gofmt

# Local outputs
BIN_DIR ?= bin
DIST_DIR ?= dist

# Go formatting settings
GOIMPORTS_FLAGS ?= -local wilaris.dev/t-cloud-public-csi-driver
GOLINES_FLAGS ?= --base-formatter=gofmt
GO_SOURCES := $(shell find . -name '*.go' \
	-not -name '*.pb.go' \
	-not -path './$(BIN_DIR)/*' \
	-not -path './$(DIST_DIR)/*')

.PHONY: build fmt fmt-check lint test vet verify

build:
	@packages="$$( $(GO) list ./... )" || exit 1; \
	if [ -z "$$packages" ]; then printf '%s\n' 'no Go packages to build'; exit 0; fi; \
	$(GO) build ./...

fmt:
	$(Q)if [ -n "$(GO_SOURCES)" ]; then \
		$(GO) tool goimports $(GOIMPORTS_FLAGS) -w $(GO_SOURCES); \
		$(GO) tool golines $(GOLINES_FLAGS) -w $(GO_SOURCES); \
	fi

fmt-check:
	$(Q)if [ -n "$(GO_SOURCES)" ]; then \
		unformatted=$$( $(GOFMT) -l $(GO_SOURCES) ); \
		if [ -n "$$unformatted" ]; then \
			echo "gofmt: these files need formatting (run 'make fmt'):" >&2; \
			echo "$$unformatted" >&2; \
			exit 1; \
		fi; \
		unformatted=$$( $(GO) tool goimports $(GOIMPORTS_FLAGS) -l $(GO_SOURCES) ); \
		if [ -n "$$unformatted" ]; then \
			echo "goimports: these files need formatting (run 'make fmt'):" >&2; \
			echo "$$unformatted" >&2; \
			exit 1; \
		fi; \
		unformatted=$$( $(GO) tool golines $(GOLINES_FLAGS) -l $(GO_SOURCES) ); \
		if [ -n "$$unformatted" ]; then \
			echo "golines: these files need formatting (run 'make fmt'):" >&2; \
			echo "$$unformatted" >&2; \
			exit 1; \
		fi; \
	fi

lint: fmt-check vet
	$(Q)packages="$$( $(GO) list ./... )" || exit 1; \
	if [ -n "$$packages" ]; then \
		$(GO) tool golangci-lint run ./...; \
	fi

test:
	@packages="$$( $(GO) list ./... )" || exit 1; \
	if [ -z "$$packages" ]; then printf '%s\n' 'no Go packages to test'; exit 0; fi; \
	$(GO) test ./...

vet:
	@packages="$$( $(GO) list ./... )" || exit 1; \
	if [ -z "$$packages" ]; then printf '%s\n' 'no Go packages to vet'; exit 0; fi; \
	$(GO) vet ./...

verify: fmt-check vet test build
