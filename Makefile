VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X github.com/Svector-anu/claude-go/internal/buildinfo.Version=$(VERSION) -X github.com/Svector-anu/claude-go/internal/buildinfo.Commit=$(COMMIT)

.PHONY: build test check mod-check
build:
	go build -ldflags '$(LDFLAGS)' -o bin/claude-go ./cmd/claude-go

test:
	go test ./...

check: mod-check
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
	go vet ./...
	go test ./...

# Validates and tests the Claude Code mod with the installed engine; skipped when claude is absent.
mod-check:
	@if command -v claude >/dev/null 2>&1; then \
		claude plugin validate integrations/claude-code/plugin && \
		claude plugin validate integrations/claude-code && \
		claude plugin test integrations/claude-code/plugin; \
	else echo "claude not installed; skipping mod checks"; fi
