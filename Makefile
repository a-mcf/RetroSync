# RetroSync build harness. Everything runs in podman; the host stays clean.
# No Go or Postgres is installed on the host.

IMAGE       ?= retrosync:dev
VERSION     ?= dev
GO_IMAGE    ?= docker.io/library/golang:1.23
PG_IMAGE    ?= docker.io/library/postgres:16
PG_NAME     ?= retrosync-test-pg
PG_PORT     ?= 5433
PG_PASSWORD ?= retrosync
PG_DB       ?= retrosync
DATABASE_URL_INT ?= postgres://postgres:$(PG_PASSWORD)@127.0.0.1:$(PG_PORT)/$(PG_DB)?sslmode=disable

# Run the Go toolchain in a throwaway container with the repo bind-mounted.
# A persistent module cache volume keeps repeat runs fast.
GORUN = podman run --rm \
	-v $(PWD):/src:Z \
	-v retrosync-gocache:/go/pkg/mod \
	-w /src \
	$(GO_IMAGE)

# Outside the repo on purpose: the repo is mounted :Z (private SELinux label)
# into the Go containers, and nesting the mesh trees inside it makes that
# relabel collide with the per-instance mounts. See scripts/syncthing-env.sh.
ST_ROOT ?= /tmp/retrosync-syncthing-test

.PHONY: build test test-integration test-syncthing vet fmt-check tidy clean ci verify

## ci: run every gate in order (fmt, vet, unit, integration, image build). The one command that verifies a slice.
ci: fmt-check vet test test-integration build
	@echo ">> CI OK: fmt, vet, unit, integration, build all passed"

## verify: alias for ci.
verify: ci

## build: build the production container image.
build:
	podman build --build-arg VERSION=$(VERSION) -t $(IMAGE) -f Containerfile .

## test: run unit tests in a container. No database required.
test:
	$(GORUN) go test ./...

## vet: run go vet in a container.
vet:
	$(GORUN) go vet ./...

## fmt-check: fail if any file is not gofmt-clean.
fmt-check:
	$(GORUN) sh -c 'out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi'

## tidy: run go mod tidy in a container (updates go.mod/go.sum).
tidy:
	$(GORUN) go mod tidy

## test-integration: spin up Postgres, run integration-tagged tests, tear down.
test-integration:
	@echo ">> starting $(PG_IMAGE) as $(PG_NAME)"
	-podman rm -f $(PG_NAME) >/dev/null 2>&1
	podman run -d --name $(PG_NAME) \
		-e POSTGRES_PASSWORD=$(PG_PASSWORD) \
		-e POSTGRES_DB=$(PG_DB) \
		-p $(PG_PORT):5432 \
		$(PG_IMAGE)
	@echo ">> waiting for Postgres to be ready"
	@for i in $$(seq 1 60); do \
		if podman exec $(PG_NAME) pg_isready -U postgres -d $(PG_DB) >/dev/null 2>&1; then \
			echo "   ready"; break; \
		fi; \
		sleep 1; \
		if [ $$i -eq 60 ]; then echo "Postgres did not become ready" >&2; podman rm -f $(PG_NAME); exit 1; fi; \
	done
	@echo ">> running integration tests"
	@set -e; \
	podman run --rm \
		--network host \
		-v $(PWD):/src:Z \
		-v retrosync-gocache:/go/pkg/mod \
		-w /src \
		-e DATABASE_URL="$(DATABASE_URL_INT)" \
		$(GO_IMAGE) go test -tags integration ./... ; \
	status=$$?; \
	echo ">> tearing down $(PG_NAME)"; \
	podman rm -f $(PG_NAME) >/dev/null 2>&1; \
	exit $$status

## test-syncthing: spin up a throwaway syncthing mesh, run syncthing-tagged
## end-to-end tests against it, tear down. NOT part of `ci` — it is slower than
## the rest of the suite and pulls a container image. Run it when touching the
## write path, the fan-out, or anything about how RetroSync and syncthing share
## a directory; that boundary is where the failures CI cannot see live.
test-syncthing:
	@echo ">> starting syncthing test mesh"
	@ST_ROOT="$(ST_ROOT)" ./scripts/syncthing-env.sh up
	@echo ">> running syncthing end-to-end tests"
	@set -e; \
	podman run --rm \
		--network host \
		-v $(PWD):/src:Z \
		-v retrosync-gocache:/go/pkg/mod \
		-v $(ST_ROOT):$(ST_ROOT):z \
		-w /src \
		-e ST_ROOT="$(ST_ROOT)" \
		$(GO_IMAGE) go test -tags syncthing -count=1 -v ./internal/syncthinge2e/... ; \
	status=$$?; \
	echo ">> tearing down syncthing test mesh"; \
	ST_ROOT="$(ST_ROOT)" ./scripts/syncthing-env.sh down; \
	exit $$status

## clean: remove test containers and scratch trees if present.
clean:
	-podman rm -f $(PG_NAME) >/dev/null 2>&1
	-ST_ROOT="$(ST_ROOT)" ./scripts/syncthing-env.sh down >/dev/null 2>&1
