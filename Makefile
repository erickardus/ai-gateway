BINARY := bin/gateway
PKG    := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all
all: vet test build

.PHONY: build
build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/gateway

.PHONY: run
run: build
	$(BINARY) -config config/gateway.yaml

.PHONY: test
test:
	go test -race $(PKG)

.PHONY: test-short
test-short:
	go test $(PKG)

.PHONY: cover
cover:
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -1

# The body editors, the breakpoint detector and the usage parser are where a
# malformed input becomes a rewritten prompt or a wrong bill rather than a failed
# request, so each of them is fuzzed.
.PHONY: fuzz
fuzz:
	go test ./internal/jsonx -run Fuzz -fuzz FuzzSetTopLevelString -fuzztime 60s
	go test ./internal/jsonx -run Fuzz -fuzz FuzzEdit -fuzztime 60s
	go test ./internal/jsonx -run Fuzz -fuzz FuzzContainsObjectKey -fuzztime 60s
	go test ./internal/promptcache -run Fuzz -fuzz FuzzInject -fuzztime 60s
	go test ./internal/provider -run Fuzz -fuzz FuzzUsageAccounting -fuzztime 60s

.PHONY: vet
vet:
	go vet $(PKG)

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then echo "unformatted files:"; echo "$$unformatted"; exit 1; fi

.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run || echo "golangci-lint not installed; skipping"

.PHONY: docker
docker:
	docker build -t ai-gateway:$(VERSION) .

.PHONY: clean
clean:
	rm -rf bin coverage.out
