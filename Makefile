# Verbosity
V ?= 0
Q := $(if $(filter 1,$(V)),,@)

GO ?= go
GOFMT ?= gofmt

# Local outputs
BIN_DIR ?= bin
DIST_DIR ?= dist
BINARY ?= t-cloud-csi-driver
CMD_PKG ?= ./cmd/t-cloud-csi-driver

# Build identity. An -X symbol path that matches nothing links cleanly and stamps nothing, so the
# package path comes from go.mod.
MODULE := $(shell $(GO) list -m)
VERSION_PKG := $(MODULE)/internal/version

# A build that cannot see the repository reports dev, like an unstamped binary.
GIT_DESCRIBE := $(shell git describe --tags --dirty 2>/dev/null)
VERSION ?= $(if $(GIT_DESCRIBE),$(GIT_DESCRIBE),dev)
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)

# Taken from the HEAD committer date, so one commit always builds to the same bytes. The date -d
# and date -r pair covers GNU and BSD.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || date -u +%s)
BUILD_DATE ?= $(shell date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
                   || date -u -r $(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ)

# -s -w drops the symbol table and DWARF; function names stay in the pclntab, so panic tracebacks
# survive.
GO_LDFLAGS := -s -w \
	-X '$(VERSION_PKG).version=$(VERSION)' \
	-X '$(VERSION_PKG).commit=$(GIT_COMMIT)' \
	-X '$(VERSION_PKG).buildDate=$(BUILD_DATE)'

# Container image. IMAGE_REPO names the registry path to push to and has no default; unset,
# the image only gets a local name. IMAGE_SOURCE names the repository the image was built from and
# has no default either, because this tree can be checked out from a mirror. Image tags cannot
# carry the semver build separator, so + becomes _.
CONTAINER_TOOL ?= docker
CONTAINERFILE ?= Containerfile
IMAGE_REPO ?=
IMAGE_SOURCE ?=
IMAGE_TAG ?= $(subst +,_,$(VERSION))
IMAGE := $(if $(IMAGE_REPO),$(IMAGE_REPO),$(BINARY)):$(IMAGE_TAG)

# Committing changes VERSION and GIT_COMMIT without touching a source file, so the binary also
# depends on a record of the identity it was stamped with.
BUILD_ID := $(VERSION) $(GIT_COMMIT) $(BUILD_DATE)
BUILD_ID_FILE := $(BIN_DIR)/.build-id

# Go formatting settings
GOIMPORTS_FLAGS ?= -local git.wilaris.dev/t-cloud-public-csi-driver
GOLINES_FLAGS ?= --base-formatter=gofmt
GO_SOURCES := $(shell find . -name '*.go' \
	-not -name '*.pb.go' \
	-not -path './$(BIN_DIR)/*' \
	-not -path './$(DIST_DIR)/*')

# The binary itself is a file target, so a repeat build with no source change does nothing.
.PHONY: build clean fmt fmt-check force image image-push image-smoke lint smoke test vet verify

build: $(BIN_DIR)/$(BINARY)

$(BIN_DIR)/$(BINARY): $(GO_SOURCES) go.mod go.sum $(BUILD_ID_FILE)
	$(Q)mkdir -p $(BIN_DIR)
	$(Q)CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o $@ $(CMD_PKG)

# Rewritten only when the identity actually changes, so the binary is relinked on a new commit and
# left alone otherwise.
$(BUILD_ID_FILE): force
	$(Q)mkdir -p $(BIN_DIR)
	$(Q)printf '%s\n' '$(BUILD_ID)' | cmp -s - $@ || printf '%s\n' '$(BUILD_ID)' > $@

# Never exists as a file, so targets depending on it always run their recipe.
force:

clean:
	$(Q)rm -rf $(BIN_DIR) $(DIST_DIR)

# Fails when an -X symbol path stops matching, which the linker itself will not report.
# LOG_LEVEL silences the startup record so only the version line reaches stdout.
smoke: build
	$(Q)out="$$( LOG_LEVEL=error $(BIN_DIR)/$(BINARY) --version )" || exit 1; \
	case "$$out" in \
		*"$(VERSION)"*) ;; \
		*) echo "--version did not report $(VERSION): $$out" >&2; exit 1;; \
	esac

# The image targets need a container daemon, so none of them is part of verify.
image:
	$(Q)$(CONTAINER_TOOL) build -f $(CONTAINERFILE) \
		--build-arg VERSION='$(VERSION)' \
		--build-arg GIT_COMMIT='$(GIT_COMMIT)' \
		--build-arg BUILD_DATE='$(BUILD_DATE)' \
		--build-arg IMAGE_SOURCE='$(IMAGE_SOURCE)' \
		-t $(IMAGE) .

# Runs --version inside the image, so a stamping mistake in the Containerfile fails here.
image-smoke: image
	$(Q)out="$$( $(CONTAINER_TOOL) run --rm -e LOG_LEVEL=error $(IMAGE) --version )" || exit 1; \
	case "$$out" in \
		*"$(VERSION)"*) ;; \
		*) echo "image --version did not report $(VERSION): $$out" >&2; exit 1;; \
	esac

# Pushing publishes the image outside this machine.
image-push: image
	$(Q)test -n "$(IMAGE_REPO)" || { \
		echo "set IMAGE_REPO to the registry path to push to," \
			"e.g. make image-push IMAGE_REPO=registry.example.com/storage/$(BINARY)" >&2; \
		exit 1; \
	}
	$(Q)$(CONTAINER_TOOL) push $(IMAGE)

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

verify: fmt-check vet test build smoke
