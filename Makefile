BINARY := bin/gateway
PKG    := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

CONFIG  ?= config/gateway.yaml
# The gateway expands ${VAR} out of its environment when it loads the config,
# and the local one names MOONSHOT_API_KEY, GATEWAY_MASTER_KEY and DEV_KEY.
# Sourcing this is what stops a run from failing on an unset variable that is
# sitting in a file two lines away. Optional: a deployment that sets its
# environment some other way simply has no .env to source.
ENV_FILE ?= .env
# Both live under bin/, which is gitignored, so neither the pid of a local
# process nor its log can be committed.
PIDFILE := bin/gateway.pid
LOGFILE := bin/gateway.log

# Sourced at the head of every recipe that starts the gateway. set -a exports
# what the file assigns, and the [ -f ] guard keeps a checkout without a .env
# from failing here rather than at the first missing variable.
load_env = set -a; [ -f $(ENV_FILE) ] && . ./$(ENV_FILE); set +a;

# Finds a gateway serving this checkout's config that make did not start. See
# the note on `stop` for why the pattern is shaped this way.
pgrep_gateway = pgrep -f "[g]ateway -config $(CONFIG)" | tr '\n' ' '

.PHONY: all
all: vet test ui build

.PHONY: build
build:
	go build -ldflags "-X main.version=$(VERSION)" -o $(BINARY) ./cmd/gateway

# The admin UI is a Vite build embedded into the binary by internal/ui. It is a
# separate target rather than a prerequisite of build so that a Go-only change
# does not need node installed; a binary built without it serves a page saying
# to run this.
.PHONY: ui
ui:
	cd web && npm ci && npm run build

# Serves the UI with hot reload against a gateway already running on :4000.
.PHONY: ui-dev
ui-dev:
	cd web && npm run dev

# Runs in the foreground, which is what you want while reading the log. Use
# `make start` for the background form.
.PHONY: run
run: build
	@$(load_env) exec $(BINARY) -config $(CONFIG)

# Starts the gateway in the background and records its pid, so that `make stop`
# and `make restart` have something to act on. Any gateway already running is
# stopped first: two processes on one addr means the second dies at bind time
# with the first still serving, which looks from the outside like a restart that
# silently did nothing.
.PHONY: start
start: build
	@$(MAKE) --no-print-directory stop
	@$(load_env) $(BINARY) -config $(CONFIG) >> $(LOGFILE) 2>&1 & echo $$! > $(PIDFILE)
	@echo "gateway started (pid $$(cat $(PIDFILE))) — logging to $(LOGFILE)"

# Stops the gateway started by `make start`. The pgrep fallback catches one
# started by hand, which is every gateway running before these targets existed.
#
# It matches the config path, not the process name. A machine can be running
# several gateways — a fleet test on other ports is the usual reason — and
# matching on the name alone would stop all of them, including ones this
# checkout never started. The bracket around the first letter keeps the pattern
# from matching the shell that is running this recipe, which has the pattern
# itself on its command line.
.PHONY: stop
stop:
	@if [ -f $(PIDFILE) ] && kill -0 $$(cat $(PIDFILE)) 2>/dev/null; then \
		kill $$(cat $(PIDFILE)) && echo "stopped gateway (pid $$(cat $(PIDFILE)))"; \
	elif pids=$$($(call pgrep_gateway)); [ -n "$$pids" ]; then \
		kill $$pids && echo "stopped gateway (pid $$pids, started outside make)"; \
	else \
		echo "no gateway running"; \
	fi; \
	rm -f $(PIDFILE)

# Rebuilds and restarts. This is the one to reach for after editing Go code or
# config/gateway.yaml: the running process holds both, so neither takes effect
# until it is replaced.
.PHONY: restart
restart: start

.PHONY: status
status:
	@if [ -f $(PIDFILE) ] && kill -0 $$(cat $(PIDFILE)) 2>/dev/null; then \
		echo "running (pid $$(cat $(PIDFILE)))"; \
	elif pids=$$($(call pgrep_gateway)); [ -n "$$pids" ]; then \
		echo "running (pid $$pids, started outside make)"; \
	else \
		echo "not running"; \
	fi

.PHONY: logs
logs:
	@tail -f $(LOGFILE)

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
	rm -rf bin coverage.out web/node_modules
	find internal/ui/dist -mindepth 1 ! -name .gitkeep -delete
