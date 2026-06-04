GO ?= go
GOFMT ?= gofmt

.PHONY: build fmt fmt-check test vet verify

build:
	@packages="$$( $(GO) list ./... )" || exit 1; \
	if [ -z "$$packages" ]; then printf '%s\n' 'no Go packages to build'; exit 0; fi; \
	$(GO) build ./...

fmt:
	@git ls-files '*.go' | while IFS= read -r file; do $(GOFMT) -w "$$file"; done

fmt-check:
	@files="$$(git ls-files '*.go')"; \
	if [ -n "$$files" ]; then \
		unformatted="$$( $(GOFMT) -l $$files )"; \
		test -z "$$unformatted" || { printf '%s\n' "$$unformatted"; exit 1; }; \
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
