# Verbosity
V ?= 0
Q := $(if $(filter 1,$(V)),,@)

# Tool entry points
GO ?= go
GOFMT ?= gofmt
CONTAINER_TOOL ?= docker
CONTAINERFILE ?= Containerfile

# Local outputs
BIN_DIR ?= bin
DIST_DIR ?= dist
BINARY ?= t-cloud-csi-driver
CMD_PKG ?= ./cmd/t-cloud-csi-driver

# MODULE is whatever go.mod records.
MODULE := $(shell $(GO) list -m)
VERSION_PKG := $(MODULE)/internal/version

# Outside a readable git tree describe is empty and VERSION becomes dev. GIT_COMMIT becomes
# unknown in the same situation.
GIT_DESCRIBE := $(shell git describe --tags --dirty 2>/dev/null)
VERSION ?= $(if $(GIT_DESCRIBE),$(GIT_DESCRIBE),dev)
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

# SOURCE_DATE_EPOCH is the HEAD committer unix time so one commit always stamps the same
# BUILD_DATE.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || date -u +%s)
BUILD_DATE ?= $(shell date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
                   || date -u -r $(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ)

# -s -w strip debug info.
GO_LDFLAGS := -s -w \
	-X '$(VERSION_PKG).version=$(VERSION)' \
	-X '$(VERSION_PKG).commit=$(GIT_COMMIT)' \
	-X '$(VERSION_PKG).buildDate=$(BUILD_DATE)'

# IMAGE_REPO is the registry path used on push. Unset, the image is tagged locally under BINARY.
# IMAGE_SOURCE is the repository the image was built from; unset, the OCI source label stays empty.
IMAGE_REPO ?=
IMAGE_SOURCE ?=
IMAGE_TAG ?= $(subst +,_,$(VERSION))
IMAGE := $(if $(IMAGE_REPO),$(IMAGE_REPO),$(BINARY)):$(IMAGE_TAG)

# A new commit changes VERSION and GIT_COMMIT without touching a Go file. Storing the stamped
# identity beside the binary forces a relink when that identity changes and leaves the binary
# alone when it has not.
BUILD_ID := $(VERSION) $(GIT_COMMIT) $(BUILD_DATE)
BUILD_ID_FILE := $(BIN_DIR)/.build-id

# Live-proof sources
E2E_TAG ?= e2e
E2E_PKG ?= ./test/e2e
E2E_DIST ?= $(DIST_DIR)/conformance
E2E_BINARY ?= t-cloud-csi-conformance
E2E_PROFILE ?= proof
E2E_GOOS ?= linux
E2E_GOARCH ?= amd64

# Go formatting settings
GOIMPORTS_FLAGS ?= -local git.wilaris.dev/t-cloud-public-csi-driver
GOLINES_FLAGS ?= --base-formatter=gofmt
GO_SOURCES := $(shell find . -name '*.go' \
	-not -name '*.pb.go' \
	-not -path './$(BIN_DIR)/*' \
	-not -path './$(DIST_DIR)/*')

.PHONY: build clean e2e e2e-build e2e-compile e2e-list fmt fmt-check force image image-push \
	image-smoke lint smoke test vet verify

# Default goal: link the stamped driver under BIN_DIR.
build: $(BIN_DIR)/$(BINARY)

$(BIN_DIR)/$(BINARY): $(GO_SOURCES) go.mod go.sum $(BUILD_ID_FILE)
	$(Q)mkdir -p $(BIN_DIR)
	$(Q)CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o $@ $(CMD_PKG)

# force is never a file, so this recipe always runs.
$(BUILD_ID_FILE): force
	$(Q)mkdir -p $(BIN_DIR)
	$(Q)printf '%s\n' '$(BUILD_ID)' | cmp -s - $@ || printf '%s\n' '$(BUILD_ID)' > $@

force:

# Delete local build and distribution trees.
clean:
	$(Q)rm -rf $(BIN_DIR) $(DIST_DIR)

# Run the built binary and require that --version contain the VERSION this file asked the linker
# to stamp.
smoke: build
	$(Q)out="$$( LOG_LEVEL=error $(BIN_DIR)/$(BINARY) --version )" || exit 1; \
	case "$$out" in \
		*"$(VERSION)"*) ;; \
		*) echo "--version did not report $(VERSION): $$out" >&2; exit 1;; \
	esac

# Link the live-proof package into a standalone binary and copy the stamped driver beside it.
e2e-build: build
	$(Q)mkdir -p $(E2E_DIST)
	$(Q)CGO_ENABLED=0 GOOS=$(E2E_GOOS) GOARCH=$(E2E_GOARCH) \
		$(GO) test -c -tags $(E2E_TAG) -trimpath -ldflags "$(GO_LDFLAGS)" \
		-o $(E2E_DIST)/$(E2E_BINARY) $(E2E_PKG)
	$(Q)cp $(BIN_DIR)/$(BINARY) $(E2E_DIST)/$(BINARY)

# Execute the asset under one declared profile. Creates, attaches, formats, mounts and deletes
# real volumes. Outside verify; requires explicit authorization on an approved instance.
e2e: e2e-build
	$(Q)cd $(E2E_DIST) && ./$(E2E_BINARY) -profile=$(E2E_PROFILE) -driver-binary=./$(BINARY)

# Compile-only check of the tagged sources; output goes to /dev/null. Catches e2e build breakage
# before a live run does.
e2e-compile:
	$(Q)CGO_ENABLED=0 GOOS=$(E2E_GOOS) GOARCH=$(E2E_GOARCH) \
		$(GO) test -c -tags $(E2E_TAG) -o /dev/null $(E2E_PKG)

# Print the clause catalogue. Runs offline, so it shows what a run would check before one is
# approved.
e2e-list: e2e-build
	$(Q)cd $(E2E_DIST) && ./$(E2E_BINARY) -list-checks

# Needs a container daemon. The image targets stay out of verify.
image:
	$(Q)$(CONTAINER_TOOL) build -f $(CONTAINERFILE) \
		--build-arg VERSION='$(VERSION)' \
		--build-arg GIT_COMMIT='$(GIT_COMMIT)' \
		--build-arg BUILD_DATE='$(BUILD_DATE)' \
		--build-arg IMAGE_SOURCE='$(IMAGE_SOURCE)' \
		-t $(IMAGE) .

# Run --version inside the image to catch Containerfile stamping mistakes before a push.
image-smoke: image
	$(Q)out="$$( $(CONTAINER_TOOL) run --rm -e LOG_LEVEL=error $(IMAGE) --version )" || exit 1; \
	case "$$out" in \
		*"$(VERSION)"*) ;; \
		*) echo "image --version did not report $(VERSION): $$out" >&2; exit 1;; \
	esac

# Pushes to a registry. IMAGE_REPO must name the registry path.
image-push: image
	$(Q)test -n "$(IMAGE_REPO)" || { \
		echo "set IMAGE_REPO to the registry path to push to," \
			"e.g. make image-push IMAGE_REPO=registry.example.com/storage/$(BINARY)" >&2; \
		exit 1; \
	}
	$(Q)$(CONTAINER_TOOL) push $(IMAGE)

# Rewrite imports and wrap long lines in place when sources exist.
fmt:
	$(Q)if [ -n "$(GO_SOURCES)" ]; then \
		$(GO) tool goimports $(GOIMPORTS_FLAGS) -w $(GO_SOURCES); \
		$(GO) tool golines $(GOLINES_FLAGS) -w $(GO_SOURCES); \
	fi

# Fail when gofmt, goimports or golines would rewrite a file.
fmt-check:
	$(Q)if [ -n "$(GO_SOURCES)" ]; then \
		for spec in \
			"gofmt $(GOFMT)" \
			"goimports $(GO) tool goimports $(GOIMPORTS_FLAGS)" \
			"golines $(GO) tool golines $(GOLINES_FLAGS)" \
		; do \
			unformatted=$$( $${spec#* } -l $(GO_SOURCES) ); \
			if [ -n "$$unformatted" ]; then \
				echo "$${spec%% *}: these files need formatting (run 'make fmt'):" >&2; \
				echo "$$unformatted" >&2; \
				exit 1; \
			fi; \
		done; \
	fi

lint: fmt-check vet
	$(Q)packages="$$( $(GO) list ./... )" || exit 1; \
	if [ -n "$$packages" ]; then \
		$(GO) tool golangci-lint run ./...; \
	fi

# Always silenced with @, not Q, so V=1 does not echo these.
test:
	@packages="$$( $(GO) list ./... )" || exit 1; \
	if [ -z "$$packages" ]; then printf '%s\n' 'no Go packages to test'; exit 0; fi; \
	$(GO) test ./...

vet:
	@packages="$$( $(GO) list ./... )" || exit 1; \
	if [ -z "$$packages" ]; then printf '%s\n' 'no Go packages to vet'; exit 0; fi; \
	$(GO) vet ./...

# Offline gate: format, static analysis, tests, stamped binary, identity check and a compile of the
# tagged proof sources. Does not build or run the image and does not reach the cloud.
verify: fmt-check vet test build smoke e2e-compile
