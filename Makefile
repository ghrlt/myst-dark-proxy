SHELL := /usr/bin/env bash
COMPOSE := docker compose

.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help doctor up down restart logs status verify verify-kill test build fmt vet update update-check autostart autostart-off clean dist dist-windows dist-linux

help: ## Show this help
	@printf '\nmyst-relay - fail-closed proxy relay over the Mysterium network\n\n'
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@printf '\n'

doctor: ## Preflight checks (run this first)
	@scripts/doctor.sh

up: ## Build and start the stack
	@test -f .env || { echo "no .env - run: cp .env.example .env"; exit 1; }
	$(COMPOSE) up -d --build
	@printf '\nstarting up; the first connection takes a minute. Watch with: make logs\n'

down: ## Stop the stack
	$(COMPOSE) down

restart: ## Recreate the stack
	$(COMPOSE) up -d --build --force-recreate

logs: ## Follow logs
	$(COMPOSE) logs -f --tail=100

status: ## Show container and relay status
	@$(COMPOSE) ps
	@printf '\n'
	@source .env 2>/dev/null; curl -s --max-time 5 \
		"http://$${RELAY_BIND:-127.0.0.1}:$${RELAY_HEALTH_PORT:-7348}/status" || echo "relay health endpoint not answering"

verify: ## Prove traffic egresses through the exit node
	@scripts/verify.sh

verify-kill: ## Also drop the tunnel and prove traffic stops
	@scripts/verify.sh --kill

update: ## Pull a newer node image, rebuild the relay, recreate what changed
	@scripts/update.sh

update-check: ## Report what could be updated, and change nothing
	@scripts/update.sh --check

test: ## Run the relay unit tests
	cd relay && go test ./...

build: ## Build the relay binary locally
	cd relay && go build -ldflags="$(LDFLAGS)" -o ../bin/mystrelay ./cmd/mystrelay

dist: dist-windows dist-linux ## Cross-compile release binaries into dist/

dist-windows: ## Build mystrelay.exe for Windows (amd64)
	@mkdir -p dist
	cd relay && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o ../dist/mystrelay.exe ./cmd/mystrelay
	@printf '  dist/mystrelay.exe  %s  %s\n' "$$(du -h dist/mystrelay.exe | cut -f1)" "$(VERSION)"
	@sha256sum dist/mystrelay.exe | sed 's|^|  sha256 |'

dist-linux: ## Build mystrelay for Linux (amd64)
	@mkdir -p dist
	cd relay && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o ../dist/mystrelay ./cmd/mystrelay
	@printf '  dist/mystrelay      %s  %s\n' "$$(du -h dist/mystrelay | cut -f1)" "$(VERSION)"

fmt: ## Format Go sources
	cd relay && gofmt -w .

vet: ## Vet Go sources
	cd relay && go vet ./...

autostart: ## Enable start-on-boot (systemd)
	@scripts/autostart.sh enable

autostart-off: ## Disable start-on-boot
	@scripts/autostart.sh disable

clean: ## Stop the stack and delete its volume (the myst node database)
	$(COMPOSE) down -v
	rm -rf bin dist
