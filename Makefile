IMAGE ?= docker-computer:dev
DOCKER ?= sudo docker

.PHONY: gate lint unit image e2e budgets tidy

gate: lint unit image e2e budgets

lint:
	@test -z "$$(gofmt -l . | grep -v '^$$')" || (echo "gofmt needed"; gofmt -l .; exit 1)
	go vet ./...
	@command -v shellcheck >/dev/null && shellcheck scripts/*.sh image/rootfs/usr/local/bin/entrypoint.sh || true

unit:
	go test -race ./...

tidy:
	go mod tidy

image:
	$(DOCKER) build -f image/Dockerfile -t $(IMAGE) .

e2e:
	SPIKE_IMAGE=$(IMAGE) ./scripts/e2e-phase0.sh

budgets:
	SPIKE_IMAGE=$(IMAGE) ./scripts/budgets.sh