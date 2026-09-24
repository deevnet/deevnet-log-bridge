VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

IMAGE          ?= localhost/deevnet-log-bridge
ARTIFACTS_ROOT ?= /srv/deevnet-http
STAGE_DIR      := $(ARTIFACTS_ROOT)/container-images/deevnet-log-bridge
TARBALL        := deevnet-log-bridge-$(VERSION).tar

# The Pi take-home image (deevnet-image-factory, pi-backend) runs the bridge as
# a plain arm64 binary beside Mosquitto. The image factory downloads it from
# the artifact server's binaries tree.
PI_ARCH    := arm64
PI_BIN     := bin/linux-$(PI_ARCH)/deevnet-log-bridge
PI_STAGE   := $(ARTIFACTS_ROOT)/binaries/deevnet-log-bridge
PI_FILE    := deevnet-log-bridge-$(VERSION)-linux-$(PI_ARCH)

LDFLAGS := -s -w \
  -X github.com/deevnet/deevnet-log-bridge/internal/version.Version=$(VERSION) \
  -X github.com/deevnet/deevnet-log-bridge/internal/version.Commit=$(COMMIT) \
  -X github.com/deevnet/deevnet-log-bridge/internal/version.Built=$(BUILT)

.PHONY: default help test vet build build-pi image stage stage-pi clean

default: help

help:
	@echo "deevnet-log-bridge"
	@echo ""
	@echo "  test    go test ./..."
	@echo "  vet     go vet ./..."
	@echo "  build   static binary in bin/"
	@echo "  build-pi  static linux/$(PI_ARCH) binary for the Pi image"
	@echo "  stage-pi  build-pi, then install it under $(PI_STAGE) (sudo)"
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

build-pi:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(PI_ARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o $(PI_BIN) ./cmd/deevnet-log-bridge

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

stage-pi: build-pi
	@case "$(VERSION)" in *-dirty|dev) echo "refusing to stage VERSION=$(VERSION); commit and tag first" >&2; exit 1;; esac
	sudo install -d -o nginx -g nginx -m 0755 $(PI_STAGE)
	sudo install -o nginx -g nginx -m 0644 $(PI_BIN) $(PI_STAGE)/$(PI_FILE)
	sudo ln -sfn $(PI_FILE) $(PI_STAGE)/deevnet-log-bridge-latest-linux-$(PI_ARCH)
	@echo "staged $(PI_STAGE)/$(PI_FILE)"

clean:
	rm -rf bin
