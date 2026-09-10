GO ?= go
PLATFORMS := darwin/arm64 linux/amd64 linux/arm64 windows/amd64

# Windows needs two builds of the same source. A console program registered as
# a logon task shows a window at every login; a GUI-subsystem one does not, but
# a shell will not wait for it, so it is unusable as a command line. The console
# build is the CLI, and agentic-statsw.exe beside it is what the service runs.
GOOS_NOW := $(shell $(GO) env GOOS)

# What the binary reports as its version, and what a peer reads to decide
# whether this node is behind. `git describe` names an exact tag plainly and
# marks anything else -- ahead of a tag, a dirty tree, no tag at all -- which
# internal/version then declines to rank. Left unset the binary says "dev",
# which is the same answer by a different route.
#
# Stamped rather than read from debug.ReadBuildInfo, which the toolchain fills
# in for free and which -trimpath does not strip. Two reasons it loses here:
# it never reports the tag, only a pseudo-version that increments the patch
# (so a commit after v1.2.3 reads as v1.2.4-... and would outrank the release
# it follows), and it needs a .git directory at build time, which a release
# cross-compiled from a tarball does not have.
#
# Computed without a shell redirect or `||`, because `make build` otherwise
# stops working wherever cmd.exe is the shell -- which is every Windows machine
# that has make but no POSIX sh, and this project ships on Windows. The empty
# check is make's own, so it needs no shell at all.
# $(or ...) is a make builtin, not a shell operator, so this keeps working
# where cmd.exe is the shell. Both it and VERSION_LDFLAGS are deferred rather
# than expanded at parse time: `git describe` costs ~50ms on Windows, and a
# simply-expanded assignment would charge it to `make test`, `make lint` and
# `make clean`, none of which stamp anything.
VERSION ?= $(or $(shell git describe --tags --always --dirty),dev)
VERSION_LDFLAGS = -X main.buildVersion=$(VERSION)

# `go build -o` writes exactly the name it is given, with no .exe appended, so
# a Windows build without this leaves an extensionless binary beside a stale
# agentic-stats.exe -- and the stale one is what a shell finds.
ifeq ($(GOOS_NOW),windows)
EXE := .exe
endif

.PHONY: check fmt vet lint test build release version ldflags clean

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
	$(GO) build -ldflags "$(VERSION_LDFLAGS)" -o bin/agentic-stats$(EXE) ./cmd/agentic-stats
ifeq ($(GOOS_NOW),windows)
	$(GO) build -ldflags "$(VERSION_LDFLAGS) -H windowsgui" -o bin/agentic-statsw.exe ./cmd/agentic-stats
endif

## version: print the version a build made now would report
##
## Single-sources the value for anything outside this file that needs it, so
## the `git describe` invocation lives in exactly one place.
version:
	@echo $(VERSION)

## ldflags: the version stamp a build made now would carry
##
## Separate from `version` because the flag name is the fragile half: -X
## against a symbol that no longer exists is silently ignored rather than an
## error, so a build spelling it by hand keeps succeeding while quietly
## shipping an unstamped binary.
ldflags:
	@echo $(VERSION_LDFLAGS)

## release: static binaries for every supported machine in the fleet
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "-s -w $(VERSION_LDFLAGS)" \
			-o dist/agentic-stats-$$os-$$arch$$ext ./cmd/agentic-stats || exit 1; \
		if [ "$$os" = "windows" ]; then \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
				$(GO) build -trimpath -ldflags "-s -w -H windowsgui $(VERSION_LDFLAGS)" \
				-o dist/agentic-statsw-$$os-$$arch$$ext ./cmd/agentic-stats || exit 1; \
		fi; \
	done

clean:
	rm -rf bin dist
