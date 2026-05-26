.PHONY: build test ui ui-dev dev clean tidy lint smokeping2gosmokeping build-all deploy

GO         ?= go
BIN        ?= gosmokeping
PKG        ?= github.com/tumult/gosmokeping/cmd/gosmokeping
UI_DIR     ?= ui
# VERSION is stamped into the binary so /api/v1/health and slave /register
# reports identify deployed builds. Derived from git when available; falls
# back to "dev" so a clean checkout without git still builds. Operators
# overriding `make build VERSION=1.2.3` get their literal string.
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    ?= -s -w -X main.version=$(VERSION)

build: ui
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BIN) $(PKG)

build-nui:
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BIN) $(PKG)

test:
	$(GO) test ./...

test-integration:
	@if [ -z "$$CLICKHOUSE_ADDR" ]; then \
		echo "set CLICKHOUSE_ADDR=host:9000 (optionally CLICKHOUSE_USERNAME / CLICKHOUSE_PASSWORD / CLICKHOUSE_DATABASE)"; exit 1; \
	fi
	$(GO) test -tags=integration ./...

ui:
	cd $(UI_DIR) && npm install && npm run build

ui-dev:
	cd $(UI_DIR) && npm run dev

dev:
	$(GO) run $(PKG) -config config.json -log-level debug

tidy:
	$(GO) mod tidy

lint:
	$(GO) vet ./...

clean:
	rm -f $(BIN)
	rm -rf internal/ui/dist $(UI_DIR)/node_modules
	mkdir -p internal/ui/dist && touch internal/ui/dist/.gitkeep

# Grant CAP_NET_RAW so ICMP works without running as root.
setcap: build
	sudo setcap cap_net_raw+ep ./$(BIN)

smokeping2gosmokeping:
	$(GO) build -ldflags="$(LDFLAGS)" -o smokeping2gosmokeping ./cmd/smokeping2gosmokeping

build-all: build smokeping2gosmokeping

# Rebuild the container image from the current working tree and roll the
# service. Intended for the host that already has ClickHouse running
# natively; see docker-compose.yml for the one-time setup checklist.
# Typical usage on the deploy VM:
#     git pull && make deploy
deploy:
	@if [ ! -f config.json ]; then \
		echo "config.json not found — copy config.example.json and edit it first"; exit 1; \
	fi
	@if [ ! -f .env ]; then \
		echo ".env not found — copy .env.example and fill in CH_PASSWORD + CLUSTER_TOKEN"; exit 1; \
	fi
	docker compose build
	docker compose up -d
	docker compose ps
