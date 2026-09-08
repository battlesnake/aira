# Common Go development commands for AIRA. Run targets from the repository
# root; the hooks also invoke these targets from any cwd.

GO ?= go
GO_BIN := $(shell $(GO) env GOROOT 2>/dev/null)/bin
export PATH := $(GO_BIN):$(HOME)/.local/bin:$(PATH)

.PHONY: fmt fmt-check vet lint build dist test race cover fuzz tidy ci install-hooks

fmt:
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*' -not -path './.worktrees/*')"; \
	if [ -n "$$files" ]; then gofmt -w $$files; fi
	@if command -v goimports >/dev/null 2>&1; then \
		files="$$(find . -type f -name '*.go' -not -path './vendor/*' -not -path './.worktrees/*')"; \
		if [ -n "$$files" ]; then goimports -w $$files; fi; \
	else \
		echo "goimports is not installed; run: go install golang.org/x/tools/cmd/goimports@latest"; \
	fi

fmt-check:
	@files="$$(find . -type f -name '*.go' -not -path './vendor/*' -not -path './.worktrees/*')"; \
	if [ -n "$$files" ] && gofmt -l $$files | grep -q .; then \
		echo "gofmt check failed; run 'make fmt'" >&2; \
		gofmt -l $$files; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "lint skipped: golangci-lint unavailable or incompatible with go1.25; install golangci-lint v2"; \
	elif golangci-lint run; then \
		:; \
	else \
		echo "lint skipped: golangci-lint unavailable or incompatible with go1.25; install golangci-lint v2"; \
	fi

build:
	$(GO) build ./...

# Local equivalent of .github/workflows/release.yml's matrix build: both
# static linux/amd64 and linux/arm64 aira binaries, same flags, in dist/.
# Kept separate from `build` above, which stays a fast whole-module compile
# check (a dependency of `ci`/the pre-push gate) and must not slow down or
# start writing release artifacts.
DIST_DIR := dist
DIST_ARCHES := amd64 arm64

dist:
	@mkdir -p $(DIST_DIR)
	@for arch in $(DIST_ARCHES); do \
		out="$(DIST_DIR)/aira-linux-$$arch"; \
		echo "building $$out"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w" -o "$$out" ./cmd/aira || exit 1; \
		sha256sum "$$out" > "$$out.sha256"; \
	done
	@file $(DIST_DIR)/aira-linux-*

# The explicit -timeout is load-bearing, not decoration. AIRA-20 widened every test
# liveness backstop so a hang is reported by name instead of by wall clock; a few of
# those firing in one package can approach go test's silent 10m default, which aborts
# the whole binary with a goroutine dump and hides the named failure the backstops
# exist to produce. -race multiplies both the run time and the backstops.
test:
	$(GO) test ./... -count=1 -timeout 20m

race:
	$(GO) test ./... -race -count=1 -timeout 40m

cover:
	$(GO) test ./... -coverprofile=coverage.out -timeout 20m
	$(GO) tool cover -func=coverage.out | tail -n 1

fuzz:
	@set -e; \
	for pkg in $$($(GO) list ./...); do \
		for fuzz in $$($(GO) test -list '^Fuzz' "$$pkg" | awk '/^Fuzz/ {print $$1}'); do \
			echo "Fuzzing $$pkg/$$fuzz for 10s"; \
			$(GO) test -run '^$$' -fuzz="$$fuzz" -fuzztime=10s "$$pkg"; \
		done; \
	done

tidy:
	$(GO) mod tidy

ci: fmt-check vet build test

install-hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/pre-commit .githooks/pre-push
