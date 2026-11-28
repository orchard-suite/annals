SHELL := /bin/bash
VERSION := $(shell cut -d' ' -f1 VERSION)
LDFLAGS := -s -w -X main.version=$(VERSION)
IMAGE := ghcr.io/orchard-suite/annals
# The ClickHouse the tests and `make run` use. Keep it on the version a
# deployment runs.
CLICKHOUSE_IMAGE := clickhouse/clickhouse-server:26.3.59.10
LOCAL_CLICKHOUSE := annals-clickhouse
export ANNALS_TEST_CLICKHOUSE ?= clickhouse://default:local@localhost:19000

.PHONY: check test clickhouse build docker run land

# Everything that must pass before a commit.
check: clickhouse
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: the files above need formatting" >&2; exit 1; }
	go vet ./...
	go test ./...

test: clickhouse
	go test ./...

# A local ClickHouse on :19000, started once and left running.
clickhouse:
	@if [ "$$(docker inspect -f '{{.State.Running}}' $(LOCAL_CLICKHOUSE) 2>/dev/null)" != "true" ]; then \
	  docker start $(LOCAL_CLICKHOUSE) >/dev/null 2>&1 || \
	  docker run -d --name $(LOCAL_CLICKHOUSE) -p 19000:9000 -e CLICKHOUSE_PASSWORD=local \
	    --ulimit nofile=262144:262144 $(CLICKHOUSE_IMAGE) >/dev/null; \
	fi; \
	for i in $$(seq 60); do \
	  docker exec $(LOCAL_CLICKHOUSE) clickhouse-client --password local -q 'SELECT 1' >/dev/null 2>&1 && exit 0; \
	  sleep 1; \
	done; echo "the local ClickHouse did not answer in 60 seconds" >&2; exit 1

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/annalsd ./cmd/annalsd
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/annals-m365 ./cmd/annals-m365

docker:
	docker build --platform linux/amd64 -t $(IMAGE):$(VERSION) .

# A local server on :8080 (the board) and :8081 (the API and MCP), on the
# local ClickHouse. The board needs the front door's headers, for example:
#   curl -H 'X-Forwarded-Email: you@example.com' -H 'X-Forwarded-Groups: Admin' localhost:8080/
run: build clickhouse
	PUBLIC_URL=http://localhost:8080 CLICKHOUSE_URL=$(ANNALS_TEST_CLICKHOUSE)/annals ./dist/annalsd

# Lands on main: release-damnit runs the config's check on the tree that
# lands and signs its proof. From a worktree: make land BRANCH=<branch>.
land:
	release-damnit land $(BRANCH)
