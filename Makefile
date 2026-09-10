GO ?= go
PLATFORMS := darwin/arm64 linux/amd64 linux/arm64 windows/amd64

# Windows only: link for the GUI subsystem, so the service started at logon has
# no console window. A console-subsystem program is given one by the operating
# system and no scheduler flag suppresses it. The command line is unaffected --
# it reattaches to its parent console at startup (cmd/agentic-stats/console_windows.go).
LDFLAGS_windows := -ldflags "-H windowsgui"

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
	$(GO) build $(LDFLAGS_$(shell $(GO) env GOOS)) -o bin/agentic-stats ./cmd/agentic-stats

## release: static binaries for every supported machine in the fleet
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; gui=""; \
		if [ "$$os" = "windows" ]; then ext=".exe"; gui=" -H windowsgui"; fi; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "-s -w$$gui" \
			-o dist/agentic-stats-$$os-$$arch$$ext ./cmd/agentic-stats || exit 1; \
	done

clean:
	rm -rf bin dist
