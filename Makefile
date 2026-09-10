GO ?= go
PLATFORMS := darwin/arm64 linux/amd64 linux/arm64 windows/amd64

# Windows needs two builds of the same source. A console program registered as
# a logon task shows a window at every login; a GUI-subsystem one does not, but
# a shell will not wait for it, so it is unusable as a command line. The console
# build is the CLI, and agentic-statsw.exe beside it is what the service runs.
GOOS_NOW := $(shell $(GO) env GOOS)

# `go build -o` writes exactly the name it is given, with no .exe appended, so
# a Windows build without this leaves an extensionless binary beside a stale
# agentic-stats.exe -- and the stale one is what a shell finds.
ifeq ($(GOOS_NOW),windows)
EXE := .exe
endif

.PHONY: check fmt vet lint test build release clean

check: fmt vet lint test

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

## lint: zero-warning policy — fix the cause, never //nolint
lint:
	golangci-lint run

test:
	$(GO) test ./...

build:
	$(GO) build -o bin/agentic-stats$(EXE) ./cmd/agentic-stats
ifeq ($(GOOS_NOW),windows)
	$(GO) build -ldflags "-H windowsgui" -o bin/agentic-statsw.exe ./cmd/agentic-stats
endif

## release: static binaries for every supported machine in the fleet
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "-s -w" \
			-o dist/agentic-stats-$$os-$$arch$$ext ./cmd/agentic-stats || exit 1; \
		if [ "$$os" = "windows" ]; then \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				$(GO) build -trimpath -ldflags "-s -w -H windowsgui" \
				-o dist/agentic-statsw-$$os-$$arch$$ext ./cmd/agentic-stats || exit 1; \
		fi; \
	done

clean:
	rm -rf bin dist
