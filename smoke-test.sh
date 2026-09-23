#!/bin/bash
# Smoke test for the log bridge: does it carry a device's log line to the store,
# and is its account confined to doing only that?
#
# Stands up a throwaway VerneMQ and its auth database, writes the bridge's
# account with THE SAME SQL the deevnet.mgmt `vernemq` role writes, runs this
# repository's own binary against it, and catches what it ships with a stub
# store. Nothing here touches the substrate.
#
# Why it exists: the bridge's account is the one account on the broker that no
# tenant prefix confines, and `+/log/#` is a shape nothing else uses. Whether
# VerneMQ's PostgreSQL ACL honours a single-level wildcard at the FIRST level
# is not something to find out on the live broker.
#
# KEEP=1 leaves the stack up for poking at.
#
# What it established the first time it ran, 2026-09-22:
#   - `+/log/#` works as a subscribe ACL. One pattern covers every tenant's log
#     level, and the plugin honours the + at the first level.
#   - The scope is exactly that level: a tenant's non-log topic is published
#     happily and the bridge never sees it.
#   - An empty publish_acl means the broker refuses the bridge's publishes.
set -u

SC="${WORK:-$(mktemp -d -t log-bridge-smoke-XXXXXX)}"
HERE="$(cd "$(dirname "$0")" && pwd)"
NET=lbr-smoke
BROKER=lbr-smoke-broker
DB=lbr-smoke-db
VERNEMQ_VERSION="${VERNEMQ_VERSION:-2.2.0}"
IMG=localhost/deevnet-vernemq:${VERNEMQ_VERSION}
PGIMG=docker.io/library/postgres:17.11
CLIIMG=docker.io/eclipse-mosquitto:2.0.22
# TLS is tested through a PUBLISHED port from the host network. The vernemq
# image's own smoke test records why: rootless podman's container-to-container
# path does not carry this TLS session, and testing it that way reports a
# broken broker when the broker is correct.
HOSTPORT=${HOSTPORT:-18884}
STOREPORT=${STOREPORT:-18428}

# The account, exactly as the role's defaults declare it.
BRIDGE_USER=substrate-log-bridge
BRIDGE_CID=deevnet-log-bridge
BRIDGE_PW=smoke-bridge-password-0123456789
# Overridable so the test can be checked against a WRONG ACL: with
# BRIDGE_SUB='[{"pattern": "nothing/#"}]' the subscription and carry checks must
# fail. A test nobody has seen fail is not evidence.
BRIDGE_SUB=${BRIDGE_SUB:-'[{"pattern": "+/log/#"}]'}

cleanup() {
  [ -n "${STORE_PID:-}" ] && kill "$STORE_PID" 2>/dev/null
  [ -n "${BRIDGE_PID:-}" ] && kill "$BRIDGE_PID" 2>/dev/null
  podman rm -f $BROKER $DB >/dev/null 2>&1
  podman network rm -f $NET >/dev/null 2>&1
}
[ -n "${KEEP:-}" ] || trap cleanup EXIT
cleanup

pass=0; fail=0
chk() { if [ "$2" = "1" ]; then echo "PASS  $1"; pass=$((pass+1)); else echo "FAIL  $1 -> ${3:-<silence>}"; fail=$((fail+1)); fi; }
# 1 when the text is there, 0 when it is not. Written out rather than reaching
# for `grep -qc`, which prints NOTHING and quietly turns every check into a
# failure.
has() { if printf '%s' "$2" | grep -qF -- "$1"; then echo 1; else echo 0; fi; }
hasnt() { if printf '%s' "$2" | grep -qF -- "$1"; then echo 0; else echo 1; fi; }

echo "### 1. the binary"
( cd "$HERE" && make build >/dev/null ) || { echo "FAIL: build"; exit 1; }
echo "ok: bin/deevnet-log-bridge"

echo "### 2. certificates"
mkdir -p "$SC/tls" && cd "$SC/tls"
openssl req -x509 -newkey rsa:2048 -nodes -keyout ca-key.pem -out ca.pem -days 2 \
  -subj "/CN=smoke-ca" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout broker-key.pem -out broker.csr \
  -subj "/CN=$BROKER" >/dev/null 2>&1
printf "subjectAltName=DNS:%s,DNS:localhost,IP:127.0.0.1\n" "$BROKER" > san.cnf
openssl x509 -req -in broker.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial \
  -out broker.pem -days 2 -extfile san.cnf >/dev/null 2>&1
chmod 644 broker-key.pem
# The client's own copy, mounted :ro,z. NEVER hand a client the broker's own
# TLS directory with :ro,Z - the private label relabels it and the broker
# silently loses access to its own key.
mkdir -p "$SC/clientca" && cp ca.pem "$SC/clientca/" && chmod 644 "$SC/clientca/ca.pem"
echo "ok: CA + broker cert"

echo "### 3. database and accounts"
podman network create $NET >/dev/null
podman run -d --name $DB --network $NET \
  -e POSTGRES_DB=vernemq -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=smoke \
  $PGIMG >/dev/null
for i in $(seq 1 40); do podman exec $DB pg_isready -U postgres -d vernemq >/dev/null 2>&1 && break; sleep 1; done

podman exec -i $DB psql -U postgres -d vernemq -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE TABLE vmq_auth_acl (
  mountpoint character varying(10) NOT NULL,
  client_id character varying(128) NOT NULL,
  username character varying(128) NOT NULL,
  password character varying(128),
  publish_acl json,
  subscribe_acl json,
  CONSTRAINT vmq_auth_acl_primary_key PRIMARY KEY (mountpoint, client_id, username)
);
CREATE ROLE vernemq LOGIN PASSWORD 'readonly';
GRANT CONNECT ON DATABASE vernemq TO vernemq;
GRANT USAGE ON SCHEMA public TO vernemq;
GRANT SELECT ON vmq_auth_acl TO vernemq;
SQL

# mabell's gateway, exactly as the Deevnet API writes it today: client id '*',
# two publish patterns under its own prefix, no subscribe.
podman exec -i $DB psql -U postgres -d vernemq -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
WITH x AS (SELECT ''::text AS mp, '*'::text AS cid, 'mabell-ma-bell-gw-01'::text AS usr,
           'gatewaypass'::text AS pw, gen_salt('bf')::text AS salt,
           '[{"pattern":"mabell/log/ma-bell-gw-01"},{"pattern":"mabell/phone/ma-bell-gw-01/state"}]'::json AS p,
           '[]'::json AS s)
INSERT INTO vmq_auth_acl SELECT x.mp,x.cid,x.usr,crypt(x.pw,x.salt),x.p,x.s FROM x;
SQL

# The bridge's account, with the statement the `vernemq` role runs - the upsert
# and its guard, so this covers the idempotence too.
bridge_sql() {
  cat <<SQL
INSERT INTO vmq_auth_acl
       (mountpoint, client_id, username, password, publish_acl, subscribe_acl)
VALUES ('', '$BRIDGE_CID', '$BRIDGE_USER',
        crypt('$BRIDGE_PW', gen_salt('bf')),
        '[]'::json,
        '$BRIDGE_SUB'::json)
ON CONFLICT (mountpoint, client_id, username) DO UPDATE
   SET password      = EXCLUDED.password,
       publish_acl   = EXCLUDED.publish_acl,
       subscribe_acl = EXCLUDED.subscribe_acl
 WHERE vmq_auth_acl.password IS DISTINCT FROM crypt('$BRIDGE_PW', vmq_auth_acl.password)
    OR vmq_auth_acl.publish_acl::text   IS DISTINCT FROM '[]'
    OR vmq_auth_acl.subscribe_acl::text IS DISTINCT FROM '$BRIDGE_SUB'
RETURNING 'written';
SQL
}
first=$(bridge_sql | podman exec -i $DB psql -U postgres -d vernemq -v ON_ERROR_STOP=1 -tA)
again=$(bridge_sql | podman exec -i $DB psql -U postgres -d vernemq -v ON_ERROR_STOP=1 -tA)
chk "the role's statement writes the bridge account" \
    "$(has written "$first")" "$first"
# The guard is what keeps `site.yml` from reporting changed for ever - and from
# rewriting a password hash on every run.
# The role's task is `changed_when: stdout is search('written')`, so this asks
# exactly what it asks. psql's own command tag ("INSERT 0 0") is not a change.
chk "and reports nothing the second time (idempotent)" \
    "$(hasnt written "$again")" "$again"
echo "ok: accounts written"

echo "### 4. broker"
mkdir -p "$SC/etc"
cat > "$SC/etc/vernemq.conf" <<CONF
nodename = VerneMQ@127.0.0.1
distributed_cookie = smoke
listener.ssl.default = 0.0.0.0:8883
listener.ssl.default.cafile = /vernemq/etc/tls/ca.pem
listener.ssl.default.certfile = /vernemq/etc/tls/broker.pem
listener.ssl.default.keyfile = /vernemq/etc/tls/broker-key.pem
listener.ssl.default.tls_version = tlsv1.2
listener.ssl.default.require_certificate = off
allow_anonymous = off
plugins.vmq_diversity = on
plugins.vmq_passwd = off
plugins.vmq_acl = off
vmq_diversity.auth_postgres.enabled = on
vmq_diversity.postgres.host = $DB
vmq_diversity.postgres.port = 5432
vmq_diversity.postgres.user = vernemq
vmq_diversity.postgres.password = readonly
vmq_diversity.postgres.database = vernemq
vmq_diversity.postgres.password_hash_method = crypt
vmq_diversity.postgres.ssl = off
log.console = console
log.console.level = info
CONF
podman run -d --name $BROKER --network $NET -p 127.0.0.1:$HOSTPORT:8883 \
  -v "$SC/etc/vernemq.conf:/vernemq/etc/vernemq.conf:ro,Z" \
  -v "$SC/tls:/vernemq/etc/tls:ro,Z" \
  $IMG >/dev/null
for i in $(seq 1 90); do
  podman exec $BROKER /vernemq/bin/vmq-admin listener show 2>/dev/null | grep -q "mqtts.*running" && break
  sleep 2
done
podman exec $BROKER /vernemq/bin/vmq-admin listener show 2>/dev/null | grep -q "mqtts.*running" \
  || { echo "FAIL: mqtts listener never came up"; exit 1; }
echo "ok: mqtts listener running"

echo "### 5. the stub store"
# Stands in for vmauth. The real path - a tenant header matched against a route
# the API wrote - was proven against the live store; what is under test here is
# what the BRIDGE sends, so this only has to record it faithfully.
cat > "$SC/store.py" <<'PY'
import http.server, json, sys, threading
OUT = sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0))
        body = self.rfile.read(n).decode()
        with open(OUT + '.bodies', 'a') as f:
            f.write(body)
        with open(OUT, 'a') as f:
            f.write(json.dumps({
                "path": self.path,
                "tenant": self.headers.get('X-Deevnet-Tenant'),
                "auth": self.headers.get('Authorization'),
                "body": body,
            }) + "\n")
        self.send_response(200); self.end_headers(); self.wfile.write(b'ok')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', int(sys.argv[1])), H).serve_forever()
PY
: > "$SC/received.jsonl"; : > "$SC/received.jsonl.bodies"
python3 "$SC/store.py" "$STOREPORT" "$SC/received.jsonl" & STORE_PID=$!
sleep 1
echo "ok: stub store on 127.0.0.1:$STOREPORT"

echo "### 6. the bridge"
DEEVNET_BRIDGE_BROKER_URL="ssl://127.0.0.1:$HOSTPORT" \
DEEVNET_BRIDGE_BROKER_USERNAME="$BRIDGE_USER" \
DEEVNET_BRIDGE_BROKER_PASSWORD="$BRIDGE_PW" \
DEEVNET_BRIDGE_CLIENT_ID="$BRIDGE_CID" \
DEEVNET_BRIDGE_BROKER_CA_FILE="$SC/tls/ca.pem" \
DEEVNET_BRIDGE_STORE_URL="http://127.0.0.1:$STOREPORT" \
DEEVNET_BRIDGE_STORE_TOKEN="smoke-store-token" \
DEEVNET_BRIDGE_FLUSH_INTERVAL=1s \
DEEVNET_BRIDGE_HEALTH_ADDR="127.0.0.1:19099" \
  "$HERE/bin/deevnet-log-bridge" > "$SC/bridge.log" 2>&1 & BRIDGE_PID=$!
for i in $(seq 1 30); do grep -qE '"msg":"(subscribed|the broker refused the subscription)"' "$SC/bridge.log" && break; sleep 1; done
chk "the broker accepts a subscription to +/log/# from the bridge's account" \
    "$(has '"msg":"subscribed"' "$(cat "$SC/bridge.log")")" "$(tail -3 "$SC/bridge.log")"
# A refused subscription leaves the connection UP, and the client library does
# not call it an error - so health has to be asked, not assumed. With a wrong
# ACL this is what fails first, and loudly.
code="$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:19099/healthz)"
chk "and the bridge reports itself healthy only then" \
    "$([ "$code" = "200" ] && echo 1 || echo 0)" "HTTP $code; $(tail -2 "$SC/bridge.log")"

cli() { podman run --rm --network host -v "$SC/clientca:/tls:ro,z" $CLIIMG "$@" 2>&1; }

echo "### 7. what it carries"
cli mosquitto_pub -h 127.0.0.1 -p $HOSTPORT --cafile /tls/ca.pem --insecure \
  -u mabell-ma-bell-gw-01 -P gatewaypass -i gw-smoke -q 1 \
  -t mabell/log/ma-bell-gw-01 -m "off-hook detected" >/dev/null
# Published, and NOT under log: the bridge must not carry this one.
cli mosquitto_pub -h 127.0.0.1 -p $HOSTPORT --cafile /tls/ca.pem --insecure \
  -u mabell-ma-bell-gw-01 -P gatewaypass -i gw-smoke -q 1 \
  -t mabell/phone/ma-bell-gw-01/state -m "ringing" >/dev/null
sleep 3

got="$(cat "$SC/received.jsonl")"
chk "a device's log line reaches the store" \
    "$(has 'off-hook detected' "$got")" "${got:-<nothing arrived>}"
chk "it is filed under the tenant from the TOPIC" \
    "$(has '"tenant": "mabell"' "$got")" "$got"
chk "the device is carried as a field" \
    "$(has '"device":"ma-bell-gw-01"' "$(cat "$SC/received.jsonl.bodies")")" "$(cat "$SC/received.jsonl.bodies")"
chk "the bridge presents its store token" \
    "$(has 'Bearer smoke-store-token' "$got")" "$got"
# The scope of +/log/# in one check: a message the tenant published outside
# that level was accepted by the broker and never seen by the bridge.
chk "a non-log topic is NOT carried" "$(hasnt ringing "$got")" "$got"

echo "### 8. what its account may not do"
# The bridge is stopped first. Its client id is PINNED in the account row, so a
# second client using it would evict the bridge mid-test - and every check
# below would then be measuring a reconnection rather than a permission.
kill "$BRIDGE_PID" 2>/dev/null; wait "$BRIDGE_PID" 2>/dev/null; BRIDGE_PID=""
sleep 1

# The pin itself: the same credential from another client id is refused
# outright, so a stolen password is not usable by another client.
out="$(cli mosquitto_pub -h 127.0.0.1 -p $HOSTPORT --cafile /tls/ca.pem --insecure \
  -u "$BRIDGE_USER" -P "$BRIDGE_PW" -i "$BRIDGE_CID-elsewhere" -q 1 \
  -t mabell/log/ma-bell-gw-01 -m "from elsewhere" -d)"
chk "the credential is refused from another client id" \
    "$(has 'Connection Refused: bad user name or password' "$out")" "$out"

# An empty publish_acl. A bridge that could publish could inject a log line
# attributed to any device of any tenant. Asking BOTH questions - did it
# connect, and did anything arrive - keeps a transport failure from passing as
# a policy denial.
out="$(cli mosquitto_pub -h 127.0.0.1 -p $HOSTPORT --cafile /tls/ca.pem --insecure \
  -u "$BRIDGE_USER" -P "$BRIDGE_PW" -i "$BRIDGE_CID" -q 1 \
  -t mabell/log/ma-bell-gw-01 -m "forged" -d)"
chk "the bridge's own account connects" "$(has 'CONNACK (0)' "$out")" "$out"
sleep 2
chk "and may not publish" "$(hasnt forged "$(cat "$SC/received.jsonl")")" "$out"

# Outside its one filter. The ACL is the only thing stopping this account from
# reading every tenant's whole tree, and a refused subscription is a SUBACK of
# 128 rather than a refused connection - so the check looks for exactly that.
out="$(cli mosquitto_sub -h 127.0.0.1 -p $HOSTPORT --cafile /tls/ca.pem --insecure \
  -u "$BRIDGE_USER" -P "$BRIDGE_PW" -i "$BRIDGE_CID" -t 'mabell/#' -W 3 -d)"
chk "and may not subscribe outside the log level" \
    "$([ "$(has 'SUBACK' "$out")$(has ': 128' "$out")" = "11" ] && echo 1 || echo 0)" "$out"

echo
echo "passed: $pass   failed: $fail"
[ "$fail" = "0" ] || exit 1
