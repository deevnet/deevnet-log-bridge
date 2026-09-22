# deevnet-log-bridge

Carries edge devices' log messages from the MQTT broker into each tenant's partition of the log
store ([ADR-0027](https://github.com/deevnet/deevnet-docs) §4, CHG-0021).

## What it does

A device publishes a log line to `<tenant>/log/<device>`. This bridge subscribes to `+/log/#` beside
the broker, and posts each message to the store through the authenticating proxy, into that tenant's
device partition.

```
device ──MQTT──▶ VerneMQ ──▶ deevnet-log-bridge ──HTTPS──▶ vmauth ──▶ VictoriaLogs
                 (same VM)                        header:            (index, 2)
                                                  X-Deevnet-Tenant
```

## Why the topic is the identity

The broker enforces each account's topic prefix: the Deevnet API writes that prefix itself, and an
account can publish nowhere else (ADR-0012 §10). So a message under `eds/…` was published by an
account belonging to `eds`, and **the first level of the topic is a tenant identity the broker has
already checked**.

That is the only thing this bridge takes a tenant from. A device that puts `{"tenant":"someone-else"}`
in its payload changes what is stored *in* the line, never where the line goes. There is a test
called `TestAPayloadCannotClaimAnotherTenant`, and it is the one to keep passing.

## What confines it

It holds **one** store token for every tenant. The tenant travels in a header, and the proxy matches
that header against routes the API wrote when it created each tenant, then overwrites the store's own
partition headers with the route's values. So the header *selects* among tenants that exist; it
cannot invent a partition, and a tenant the API never created has no route at all.

Its broker account may **subscribe only**, to one topic level. It never publishes.

## What it is not

- **Not in a device's path.** Stopping it loses logs. It does not stop a device working, and the
  broker goes on accepting messages either way.
- **Not a tenant's own log path.** A tenant's workloads ship their logs straight to the store with
  the tenant's own ingest token. This is only for devices, which have no such token and no route to
  the store.

## Configuration

Environment only. Both credentials arrive in a root-only env file written by the Ansible role.

| | |
|---|---|
| `DEEVNET_BRIDGE_BROKER_URL` | required, e.g. `tls://mqtt.mobile.deevnet.net:8883` |
| `DEEVNET_BRIDGE_BROKER_USERNAME` / `_PASSWORD` | required; the bridge's subscribe-only account |
| `DEEVNET_BRIDGE_BROKER_CA_FILE` | the site CA, for the broker's certificate |
| `DEEVNET_BRIDGE_CLIENT_ID` | default `deevnet-log-bridge`; its persistent session |
| `DEEVNET_BRIDGE_TOPIC` | default `+/log/#` |
| `DEEVNET_BRIDGE_STORE_URL` | required, e.g. `https://dv02obs001v01.mobile.deevnet.net:8427` |
| `DEEVNET_BRIDGE_STORE_TOKEN` | required; the bridge user's bearer token |
| `DEEVNET_BRIDGE_STORE_CA_FILE` | the site CA, for the store's certificate |
| `DEEVNET_BRIDGE_QUEUE_MAX` | default 10000 lines |
| `DEEVNET_BRIDGE_FLUSH_INTERVAL` | default `2s` |
| `DEEVNET_BRIDGE_HEALTH_ADDR` | default `127.0.0.1:9099`, serving `/healthz` |

## Delivery, stated rather than implied

- It subscribes at **QoS 1** with a **persistent session**, so the broker holds messages for it while
  it restarts. How many, and for how long, is the broker's setting and not this program's promise.
- Its queue is **bounded**. When it is full the **oldest** lines are dropped and the loss is logged
  with a count, because the one thing a log pipeline must not do is lose things quietly.
- A batch the store **refuses** is not retried: the same bytes would be refused again, and retrying
  would hold up every other tenant's logs. A batch the store **errors** on is retried with backoff.

## Build

```bash
make test
make image                  # podman build localhost/deevnet-log-bridge:<version>
make stage                  # save it where the Ansible role reads it (needs a clean, tagged tree)
```

Images are **pushed, not pulled**: the messaging VM has no route back to the artifact server under
the zone policy, so the role copies the tarball to the host and loads it there.
