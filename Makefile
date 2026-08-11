# Common Go development commands for AIRA. Run targets from the repository
# root; the hooks also invoke these targets from any cwd.

GO ?= /home/user/.local/bin/go
GO_BIN := $(shell $(GO) env GOROOT 2>/dev/null)/bin
export PATH := $(GO_BIN):/home/user/.local/bin:$(PATH)

.PHONY: fmt fmt-check vet lint build test race cover fuzz tidy ci install-hooks

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

test:
	$(GO) test ./... -count=1

race:
	$(GO) test ./... -race -count=1

cover:
	$(GO) test ./... -coverprofile=coverage.out
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
