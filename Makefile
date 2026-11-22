IMAGE ?= docker-computer:dev
# Use sudo only when the current user cannot reach the Docker daemon.
DOCKER ?= $(shell docker info >/dev/null 2>&1 && echo docker || echo "sudo docker")
DEV_NAME ?= dc-dev
DEV_PORT ?= 8080
DEV_TOKEN ?= dev-token

DOCKER_RUN := $(DOCKER) run --rm -v /var/run/docker.sock:/var/run/docker.sock
DIVE       := $(DOCKER_RUN) -v $(CURDIR)/.dive-ci:/.dive-ci:ro -v $(CURDIR)/.build:/out wagoodman/dive:latest
HADOLINT   := $(DOCKER) run --rm -v $(CURDIR):/src:ro -w /src ghcr.io/hadolint/hadolint hadolint --config /src/.hadolint.yaml

.PHONY: help gate lint hadolint unit build e2e budgets dive tidy bump-base dev dev-stop dev-logs

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

gate: lint unit build e2e budgets dive ## Run every check (lint, unit, build, e2e, budgets, dive)

lint: hadolint ## gofmt, go vet, shellcheck and hadolint
	@test -z "$$(gofmt -l . | grep -v '^$$')" || (echo "gofmt needed"; gofmt -l .; exit 1)
	go vet ./...
	@command -v shellcheck >/dev/null && shellcheck scripts/*.sh docker/rootfs/usr/local/bin/entrypoint.sh || true

hadolint: ## Lint the Dockerfile
	$(HADOLINT) docker/Dockerfile

unit: ## Run unit tests with the race detector
	go test -race ./...

tidy: ## Tidy go.mod and go.sum
	go mod tidy

build: ## Build the container image
	$(DOCKER) build -f docker/Dockerfile -t $(IMAGE) .

e2e: ## Run the end-to-end script against the image
	DC_IMAGE=$(IMAGE) ./scripts/e2e.sh

budgets: ## Measure size, memory and boot time against budgets
	DC_IMAGE=$(IMAGE) ./scripts/budgets.sh

dive: ## Image efficiency gate: fails when wasted space or efficiency breaks .dive-ci
	$(DIVE) --ci $(IMAGE)

bump-base: ## Re-pin DEBIAN_IMAGE in docker/Dockerfile to the current debian:trixie-slim digest
	$(DOCKER) pull -q debian:trixie-slim >/dev/null
	@digest=$$($(DOCKER) image inspect debian:trixie-slim --format '{{index .RepoDigests 0}}' | sed 's/^debian@//'); \
	  sed -i "s|^ARG DEBIAN_IMAGE=.*|ARG DEBIAN_IMAGE=debian:trixie-slim@$$digest|" docker/Dockerfile; \
	  grep '^ARG DEBIAN_IMAGE=' docker/Dockerfile

dev: build dev-stop ## Build, run the container, and print the desktop URL
	@echo ""
	@echo "  Desktop: http://localhost:$(DEV_PORT)/desktop/?token=$(DEV_TOKEN)"
	@echo "  Stop with Ctrl-C or: make dev-stop"
	@echo ""
	$(DOCKER) run --rm --name $(DEV_NAME) \
	  -p 127.0.0.1:$(DEV_PORT):8080 \
	  -e COMPUTER_AGENT_TOKEN=$(DEV_TOKEN) \
	  $(IMAGE)

dev-stop: ## Stop the dev container if it is running
	-@$(DOCKER) rm -f $(DEV_NAME) >/dev/null 2>&1

dev-logs: ## Follow the dev container logs
	$(DOCKER) logs -f $(DEV_NAME)
