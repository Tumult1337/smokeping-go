.PHONY: build test ui ui-dev dev clean tidy lint smokeping2gosmokeping build-all deploy deploy-slave

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
	@# vite's emptyOutDir deletes the tracked .gitkeep that go:embed needs
	@# for a dist-less clone to build.
	touch internal/ui/dist/.gitkeep

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

# Rebuild the slave container from the current working tree and roll it.
# Slaves never touch ClickHouse, so the only prerequisite is a populated
# config.slave.json pointing at the master. Typical usage on a remote
# vantage point host:
#     git pull && make deploy-slave
deploy-slave:
	@if [ ! -f config.slave.json ]; then \
		echo "config.slave.json not found — copy config.slave.example.json and fill in master_url + name"; exit 1; \
	fi
	@if [ ! -f .env ]; then \
		echo ".env not found — copy .env.example and set CLUSTER_TOKEN (\$${CLUSTER_TOKEN} in config.slave.json gets substituted from here)"; exit 1; \
	fi
	@# Auto-fill the slave health-mesh address. Only fires when config.slave.json
	@# references $${ADVERTISE_IP} and .env doesn't already set it, so a value you
	@# put in .env yourself always wins and this never overwrites. The slave compose
	@# uses host networking, so the default-route source IP is the address peers
	@# reach this node on — exactly what cluster.advertise needs. Detection failing
	@# (no default route) leaves it unset, cleanly opting the slave out of the mesh.
	@if grep -qF '$${ADVERTISE_IP}' config.slave.json && ! grep -q '^ADVERTISE_IP=' .env; then \
		ip=$$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($$i=="src") print $$(i+1)}'); \
		if [ -n "$$ip" ]; then \
			echo "ADVERTISE_IP=$$ip" >> .env; \
			echo "health mesh: auto-filled ADVERTISE_IP=$$ip in .env (edit .env to override)"; \
		else \
			echo "health mesh: could not autodetect ADVERTISE_IP; set it in .env to join the mesh"; \
		fi; \
	fi
	docker compose -f docker-compose.slave.yml build
	docker compose -f docker-compose.slave.yml up -d
	docker compose -f docker-compose.slave.yml ps
