VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

IMAGE          ?= localhost/deevnet-log-bridge
ARTIFACTS_ROOT ?= /srv/deevnet-http
STAGE_DIR      := $(ARTIFACTS_ROOT)/container-images/deevnet-log-bridge
TARBALL        := deevnet-log-bridge-$(VERSION).tar

LDFLAGS := -s -w \
  -X github.com/deevnet/deevnet-log-bridge/internal/version.Version=$(VERSION) \
  -X github.com/deevnet/deevnet-log-bridge/internal/version.Commit=$(COMMIT) \
  -X github.com/deevnet/deevnet-log-bridge/internal/version.Built=$(BUILT)

.PHONY: default help test vet build image stage clean

default: help

help:
	@echo "deevnet-log-bridge"
	@echo ""
	@echo "  test    go test ./..."
	@echo "  vet     go vet ./..."
	@echo "  build   static binary in bin/"
	@echo "  image   podman build $(IMAGE):$(VERSION)"
	@echo "  stage   image, then save it under $(STAGE_DIR) (sudo)"
	@echo "  clean   remove bin/"
	@echo ""
	@echo "VERSION=$(VERSION)"

test:
	go test ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/deevnet-log-bridge ./cmd/deevnet-log-bridge

image:
	podman build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILT=$(BUILT) \
	  -t $(IMAGE):$(VERSION) \
	  -f Containerfile .

# A dirty tree is refused: what runs on a host has to be traceable to a commit.
stage: image
	@case "$(VERSION)" in *-dirty|dev) echo "refusing to stage VERSION=$(VERSION); commit and tag first" >&2; exit 1;; esac
	@mkdir -p bin
	@# podman save refuses to write over an existing archive.
	rm -f bin/$(TARBALL)
	podman save -o bin/$(TARBALL) $(IMAGE):$(VERSION)
	sudo install -d -o nginx -g nginx -m 0755 $(STAGE_DIR)
	sudo install -o nginx -g nginx -m 0644 bin/$(TARBALL) $(STAGE_DIR)/$(TARBALL)
	sudo ln -sfn $(TARBALL) $(STAGE_DIR)/deevnet-log-bridge-latest.tar
	@echo "staged $(STAGE_DIR)/$(TARBALL)"

clean:
	rm -rf bin
