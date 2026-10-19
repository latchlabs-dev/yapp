.DEFAULT_GOAL := build
.PHONY: build test lint docker-up docker-down package check-release

PYTHON ?= python3
EXTENSION_TEST_DIR := /tmp/piproxy-extension-test-dist

ORG      := github.com/earlgrey/piproxy
BIN_DIR  := bin
CMD_DIR  := cmd
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -ldflags "-X $(ORG)/internal/config.Version=$(VERSION) -X $(ORG)/internal/config.Commit=$(COMMIT) -X $(ORG)/internal/config.BuildDate=$(DATE)"
PROJECT_GO_TOOLCHAIN := go$(shell awk '$$1 == "go" { print $$2; exit }' go.mod)

# ----- Build -----

build: build-proxy build-admin

build-proxy: build-pdfextract
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BIN_DIR)/proxy ./$(CMD_DIR)/proxy

build-admin:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BIN_DIR)/admin ./$(CMD_DIR)/admin

build-pdfextract:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -o $(BIN_DIR)/pdfextract ./$(CMD_DIR)/pdfextract

build-all: build-pdfextract
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BIN_DIR)/proxy ./$(CMD_DIR)/proxy
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BIN_DIR)/admin ./$(CMD_DIR)/admin

# ----- Run -----

# Dev admin key — generated once and stable across make invocations in this shell.
DEV_ADMIN_KEY ?= $(shell echo "adm_dev_$$(openssl rand -hex 16)")

run: build-proxy
	./$(BIN_DIR)/proxy

run-admin: build-admin
	@if [ -z "$$PROXY_ADMIN_KEY" ]; then \
		echo "  → Using dev admin key: $(DEV_ADMIN_KEY)"; \
		echo "  → Set PROXY_ADMIN_KEY=<key> in your shell to use a custom key."; \
		echo ""; \
	fi
	PROXY_ADMIN_KEY="$${PROXY_ADMIN_KEY:-$(DEV_ADMIN_KEY)}" ./$(BIN_DIR)/admin

# ----- Dev (proxy + admin API + UI) -----

UI_DIR := ui
WEBUI_DIR := webui

dev: build-proxy build-admin
	@PROXY_ADMIN_KEY="$${PROXY_ADMIN_KEY:-$(DEV_ADMIN_KEY)}" bash dev.sh "$(BIN_DIR)" "$(UI_DIR)"

# ----- Test -----

test: test-go test-extension test-terminal-browser webui-test

test-go:
	go test -v -race -count=1 -coverprofile=coverage.out ./...

test-session-query:
	cd services/session-query && uv run --locked python -m unittest discover -s tests -v

build-session-query:
	docker build -t piproxy-session-query:$(VERSION) services/session-query

test-extension:
	rm -rf $(EXTENSION_TEST_DIR)
	cd ui && pnpm exec tsc -p ../extensions/tsconfig.json
	mkdir -p $(EXTENSION_TEST_DIR)/node_modules/@earendil-works/pi-ai
	printf 'function openAIStreamSimple(model, context, options) { if (globalThis.__piAiStreamSimpleImpl) return globalThis.__piAiStreamSimpleImpl(model, context, options); globalThis.__piAiStreamSimpleCalls = [...(globalThis.__piAiStreamSimpleCalls || []), { model, context, options }]; return { [Symbol.asyncIterator]: async function* () {} }; }\nexports.streamSimple = openAIStreamSimple;\nexports.openAICompletionsApi = function openAICompletionsApi() { return { streamSimple: openAIStreamSimple }; };\nexports.openAICodexResponsesApi = function openAICodexResponsesApi() { return { streamSimple: function streamSimple(model, context, options) { if (globalThis.__piAiCodexStreamSimpleImpl) return globalThis.__piAiCodexStreamSimpleImpl(model, context, options); globalThis.__piAiCodexStreamSimpleCalls = [...(globalThis.__piAiCodexStreamSimpleCalls || []), { model, context, options }]; return { [Symbol.asyncIterator]: async function* () {} }; } }; };\n' > $(EXTENSION_TEST_DIR)/node_modules/@earendil-works/pi-ai/index.js
	mkdir -p $(EXTENSION_TEST_DIR)/node_modules/typebox
	printf 'exports.Type = { Object: (properties) => ({ type: "object", properties }), Union: (anyOf, options) => ({ ...options, anyOf }), Literal: (value) => ({ const: value }), String: (options) => ({ type: "string", ...options }), Optional: (schema) => ({ ...schema, optional: true }) };\n' > $(EXTENSION_TEST_DIR)/node_modules/typebox/index.js
	node --test $(EXTENSION_TEST_DIR)/extensions/tests/*.test.js

test-terminal-browser:
	cd extensions/pi-terminal-browser && pnpm install --frozen-lockfile && pnpm test

test-short:
	go test -v -short -count=1 ./...

test-race:
	go test -v -race -count=1 ./...

coverage: test
	go tool cover -html=coverage.out -o coverage.html

# ----- Lint -----

lint: lint-go lint-extension

lint-go:
	GOTOOLCHAIN=$(PROJECT_GO_TOOLCHAIN) golangci-lint run ./...

lint-extension:
	cd ui && pnpm exec tsc -p ../extensions/tsconfig.json --noEmit
	cd extensions/pi-terminal-browser && pnpm typecheck

lint-fix:
	GOTOOLCHAIN=$(PROJECT_GO_TOOLCHAIN) golangci-lint run --fix ./...

fmt:
	go fmt ./...
	goimports -local $(ORG) -w .

# ----- Clean -----

clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html extensions/pi-terminal-browser/dist

# ----- Docker -----

docker-up:
	./examples/quickstart/build.sh
	docker compose -f examples/quickstart/compose.yml up -d --wait

docker-down:
	docker compose -f examples/quickstart/compose.yml down

docker-logs:
	docker compose -f examples/quickstart/compose.yml logs -f

# ----- Migrations -----

migrate-up:
	@if [ -z "$(DSN)" ]; then echo "Usage: make migrate-up DSN=postgres://proxy:proxy@localhost:5432/proxy?sslmode=disable"; exit 1; fi
	migrate -path migrations -database "$(DSN)" up

migrate-down:
	@if [ -z "$(DSN)" ]; then echo "Usage: make migrate-down DSN=postgres://proxy:proxy@localhost:5432/proxy?sslmode=disable"; exit 1; fi
	migrate -path migrations -database "$(DSN)" down

sync-pi-provider-catalog:
	@if [ -z "$(DSN)$${PROXY_POSTGRES_DSN}" ]; then echo "Usage: make sync-pi-provider-catalog DSN=postgres://proxy:proxy@localhost:5432/proxy?sslmode=disable"; exit 1; fi
	DSN="$(DSN)" PROXY_POSTGRES_DSN="$${PROXY_POSTGRES_DSN}" node scripts/sync-pi-provider-catalog.mjs

# ----- UI (Admin Dashboard) -----

ui-install:
	cd $(UI_DIR) && pnpm install

ui-dev:
	cd $(UI_DIR) && pnpm dev

ui-build:
	cd $(UI_DIR) && pnpm build

ui-preview:
	cd $(UI_DIR) && pnpm preview

# ----- Public web console -----

webui-install:
	cd $(WEBUI_DIR) && pnpm install

webui-dev:
	cd $(WEBUI_DIR) && pnpm dev

webui-build:
	cd $(WEBUI_DIR) && pnpm build

webui-test:
	cd $(WEBUI_DIR) && pnpm test

# Build local release archives; never publish or deploy implicitly.
package:
	$(PYTHON) scripts/package-release.py --version "$(VERSION)" --output dist

check-release:
	$(PYTHON) scripts/check-release.py

help:
	@echo "build | test | lint | docker-up | docker-down | package | check-release"
