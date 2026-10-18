GO ?= go
GOTOOLCHAIN ?= go1.26.0
export GOTOOLCHAIN

.PHONY: check test vet build build-linux clean
check: test vet

test:
	$(GO) test -mod=readonly -race ./...

vet:
	$(GO) vet -mod=readonly ./...

build:
	mkdir -p build
	$(GO) build -mod=readonly -trimpath -buildvcs=false -o build/duckduckgo-search-mcp .

build-linux:
	mkdir -p build
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -mod=readonly -trimpath -buildvcs=false -ldflags="-s -w" -o build/duckduckgo-search-mcp-linux-amd64 .

clean:
	rm -rf build
