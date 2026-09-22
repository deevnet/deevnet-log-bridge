# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`deevnet-log-bridge` carries edge devices' log messages from the MQTT broker into each tenant's
`(index, 2)` partition of the log store (ADR-0027 §4, CHG-0021). It runs as a container beside the
broker on the messaging VM.

It is **our own software**, which is why it lives here and builds its own image, like
`deevnet-provisioning-api` does. `deevnet-container-image-factory` is for third-party software whose
source we may use but whose binaries we may not; nothing we write belongs there.

## Commands

```bash
make test   # go test ./...
make vet    # go vet ./...
make build  # static binary in bin/
make image  # podman build localhost/deevnet-log-bridge:<version>
make stage  # save the image under the Builder's artifact root (needs a clean, tagged tree)
```

## Rules that are easy to get wrong

- **The tenant comes from the topic, never the payload.** The broker enforces each account's prefix
  (ADR-0012 §10), so the first topic level is an identity that has already been checked. A payload is
  content. `TestAPayloadCannotClaimAnotherTenant` exists to keep this true; if it ever fails, the
  bridge is routing one tenant's logs into another's partition.
- **One request carries one tenant's lines.** The tenant travels in a header the proxy matches, so a
  mixed batch would be filed under the wrong one. `Batch.Body` refuses one rather than sending it.
- **The bridge never publishes**, and its account has no publish grant. It is not in a device's path:
  stopping it loses logs and breaks nothing.
- **Never add a way to name a partition.** The bridge names a *tenant*; the proxy turns that into a
  partition. A partition number in this code, or in a request it sends, is a bug.
- **Loss is counted and logged, never silent.** The queue is bounded and drops the oldest under
  pressure. Keep it that way, and keep the warning.
- **Standard library first.** The only dependency is an MQTT client, because the standard library has
  none. Don't add a logging framework, a router, or an HTTP client library.
- **Base images are fully qualified** (`docker.io/...`, `gcr.io/...`). Podman refuses short names
  non-interactively.
