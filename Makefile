GO ?= go
PLATFORMS := darwin/arm64 linux/amd64 linux/arm64 windows/amd64

.PHONY: check fmt vet test build release clean

check: fmt vet test

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

build:
	$(GO) build -o bin/ ./cmd/...

## release: static binaries for every supported machine in the fleet
release:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = "windows" ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
			$(GO) build -trimpath -ldflags "-s -w" \
			-o dist/agentic-stats-agent-$$os-$$arch$$ext ./cmd/agent || exit 1; \
	done

clean:
	rm -rf bin dist
