# Built by `make image`; the Makefile passes the build arguments.
#
# Base images are fully qualified: podman refuses short names when it cannot
# prompt, which is every non-interactive build.

ARG GO_VERSION=1.25.9

FROM docker.io/library/golang:${GO_VERSION} AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/deevnet/deevnet-log-bridge/internal/version.Version=${VERSION} \
        -X github.com/deevnet/deevnet-log-bridge/internal/version.Commit=${COMMIT} \
        -X github.com/deevnet/deevnet-log-bridge/internal/version.Built=${BUILT}" \
      -o /out/deevnet-log-bridge ./cmd/deevnet-log-bridge

# Static binary, no shell, no package manager, non-root. The bridge needs none
# of them: it reads two credentials from its environment and talks to two
# services.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/deevnet-log-bridge /usr/local/bin/deevnet-log-bridge
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/deevnet-log-bridge"]
