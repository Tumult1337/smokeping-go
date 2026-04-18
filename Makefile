.PHONY: build test ui ui-dev dev clean tidy lint

GO         ?= go
BIN        ?= gosmokeping
PKG        ?= github.com/tumult/gosmokeping/cmd/gosmokeping
UI_DIR     ?= ui
LDFLAGS    ?= -s -w

build: ui
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BIN) $(PKG)

build-nui:
	$(GO) build -ldflags="$(LDFLAGS)" -o $(BIN) $(PKG)

test:
	$(GO) test ./...

test-integration:
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
