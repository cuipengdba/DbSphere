import collections
import http.server
import json
import os
from pathlib import Path
import re
import socketserver
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request


BASE = "http://127.0.0.1:8081"
STORE = "dbsphere-go-dev-store-1"
ARCHIVE = sys.argv[1]
ENV = Path.home() / ".config/dbsphere-go/dbsphere-go.env"
MUTEX = threading.Lock()
RECEIPTS = collections.defaultdict(list)
IDENTITY = re.compile(r"^[a-f0-9]{32}$")
UUID = re.compile(r"^[a-f0-9-]{36}$")


def command(*args):
    return subprocess.run(args, check=True, text=True, stdout=subprocess.PIPE).stdout.strip()


def sql(query):
    return command("docker", "exec", STORE, "psql", "-X", "-U", "dbsphere",
                   "-d", "dbsphere", "-v", "ON_ERROR_STOP=1", "-Atqc", query)


def until(label, probe, timeout=45, interval=0.5):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            value = probe()
            if value:
                return value
        except (ConnectionError, TimeoutError, urllib.error.URLError):
            pass
        time.sleep(interval)
    raise AssertionError(f"Timed out waiting for {label}")


class Webhook(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        message = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        identity = message["deliveryId"]
        if self.headers.get("Idempotency-Key") != identity or self.headers.get("X-DbSphere-Delivery-Id") != identity:
            self.send_error(400)
            return
        with MUTEX:
            RECEIPTS[identity].append(message)
            first = len(RECEIPTS[identity]) == 1
        self.send_response(503 if first else 204)
        self.end_headers()

    def log_message(self, *_):
        pass


class SlowPostgres(socketserver.BaseRequestHandler):
    def handle(self):
        time.sleep(12)


class SlowServer(socketserver.ThreadingTCPServer):
    daemon_threads = True


def start(service):
    threading.Thread(target=service.serve_forever, daemon=True).start()
    return service.server_address[1]


def request(path, method="GET", payload=None, auth=None):
    headers = {"Origin": BASE}
    if auth:
        headers.update(auth)
    body = None
    if payload is not None:
        body = json.dumps(payload).encode()
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(BASE + path, method=method, data=body, headers=headers)
    try:
        response = urllib.request.urlopen(req, timeout=6)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, response.headers, response.read()


def api(path, method="GET", payload=None, auth=None, expected=200):
    status, _, data = request(path, method, payload, auth)
    assert status == expected, (path, status, data.decode(errors="replace"))
    return json.loads(data)


def create_asset(name, address, port, auth):
    row = api("/api/assets", "POST", {"name": name, "host": address, "port": port,
                                      "database": "postgres", "username": "slow_probe",
                                      "password": "synthetic-fixture-only", "sslMode": "disable",
                                      "runtime": "linux"}, auth, 201)["asset"]
    assert UUID.fullmatch(row["id"])
    return row["id"]


def notification_for(asset_id, rule_id):
    with MUTEX:
        return next(((key, messages[0]["eventId"]) for key, messages in RECEIPTS.items()
                     if messages[0]["assetId"] == asset_id and messages[0]["ruleId"] == rule_id), None)


def wait_sample(asset_id, producer):
    return sql(f"""SELECT id::text FROM samples WHERE asset_id='{asset_id}'
        AND metrics_json#>>'{{collector,replicaId}}'='{producer}' ORDER BY collected_at DESC LIMIT 1""")


def test():
    original = ENV.read_text()
    config = dict(line.split("=", 1) for line in original.splitlines() if "=" in line)
    address = command("docker", "network", "inspect", "dbsphere-go-dev_default",
                      "--format", "{{(index .IPAM.Config 0).Gateway}}")
    webhook = http.server.ThreadingHTTPServer(("0.0.0.0", 0), Webhook)
    slow = SlowServer(("0.0.0.0", 0), SlowPostgres)
    webhook_port, slow_port = start(webhook), start(slow)
    ENV.write_text(original + f"\nDBSPHERE_GO_HA=true\nDBSPHERE_COLLECT_INTERVAL_SECONDS=15\n"
                   f"DBSPHERE_COLLECT_WORKERS=1\nDBSPHERE_WEBHOOK_URL=http://{address}:{webhook_port}\n"
                   "DBSPHERE_WEBHOOK_ALLOW_HTTP=true\nDBSPHERE_WEBHOOK_MIN_SEVERITY=info\n")
    try:
        command("bash", ARCHIVE)
        status, headers, data = request("/api/login", "POST", {"password": config["DBSPHERE_ADMIN_PASSWORD"]})
        assert status == 200, data
        auth = {"Cookie": headers["Set-Cookie"].split(";", 1)[0], "X-CSRF-Token": json.loads(data)["csrf"]}
        replica_ids = {request("/readyz")[1].get("X-DbSphere-Replica") for _ in range(6)}
        assert len(replica_ids) == 2 and all(replica and IDENTITY.fullmatch(replica) for replica in replica_ids), replica_ids

        def inspect():
            info = api("/api/admin/platform", auth=auth)
            return info if info.get("maintenance") and len([p for p in info["replicas"] if p["online"]]) == 2 else None

        cluster = until("two replicas and maintenance leader", inspect)
        original_leader = cluster["maintenance"]["owner"]
        leader = next(p for p in cluster["replicas"] if p["id"] == original_leader)
        assert leader["name"] in ("app", "replica")
        other = next(p["id"] for p in cluster["replicas"] if p["id"] != original_leader and p["online"])

        notifying = create_asset("HA notification fixture", address, 1, auth)
        delivery, event = until("first webhook attempt", lambda: notification_for(notifying, "pg_unavailable"))
        assert UUID.fullmatch(delivery) and UUID.fullmatch(event)
        for number in range(4):
            create_asset(f"HA lease {number}", address, slow_port, auth)

        def active_lease():
            return sql(f"""SELECT j.asset_id::text||'|'||j.lease_token::text||'|'||j.due_at::text
                FROM collection_jobs j JOIN assets a ON a.id=j.asset_id
                WHERE a.name LIKE 'HA lease %' AND j.lease_replica_id='{original_leader}'
                AND j.lease_until>clock_timestamp() LIMIT 1""")

        lost_asset, lease, due = until("an acquired collection lease", active_lease, interval=0.2).split("|", 2)
        assert UUID.fullmatch(lost_asset) and UUID.fullmatch(lease)
        assert re.fullmatch(r"[0-9 .:+-]+", due)
        killed_at = time.monotonic()
        command("docker", "stop", "-t", "0", "dbsphere-go-dev-" + leader["name"] + "-1")
        assert sql(f"SELECT lease_token::text FROM collection_jobs WHERE asset_id='{lost_asset}'") == lease

        def takeover():
            status, _, raw = request("/api/admin/platform", auth=auth)
            if status != 200:
                return None
            state = json.loads(raw)
            return state if (state.get("maintenance") or {}).get("owner") == other else None

        new_cluster = until("survivor becoming maintenance leader", takeover, timeout=28)
        failover = round(time.monotonic() - killed_at, 2)
        assert new_cluster["maintenance"]["generation"] > cluster["maintenance"]["generation"]
        assert request("/api/events/" + event, auth=auth)[0] == 200
        sample = until("survivor completing the expired collection lease", lambda: wait_sample(lost_asset, other), timeout=88)
        assert UUID.fullmatch(sample)
        copies = sql(f"""SELECT count(*) FROM samples WHERE asset_id='{lost_asset}'
           AND (metrics_json#>>'{{collector,scheduledFor}}')::timestamptz='{due}'::timestamptz""")
        assert copies == "1", copies
        assert sql(f"""SELECT count(*) FROM events WHERE asset_id='{notifying}'
           AND status='open' AND event_group='availability'""") == "1"
        until("retried webhook recorded as delivered", lambda: sql(
            f"SELECT status FROM notification_outbox WHERE id='{delivery}'") == "delivered")
        with MUTEX:
            assert len(RECEIPTS[delivery]) == 2, RECEIPTS[delivery]
            assert {msg["eventId"] for msg in RECEIPTS[delivery]} == {event}
        evidence = api("/api/events/" + event, auth=auth)["event"]["evidence"]
        assert any(item["kind"] == "notification_delivery" and item["payload"].get("deliveryId") == delivery
                   and item["payload"].get("status") == "delivered" for item in evidence)

        command("docker", "stop", "-t", "2", STORE)
        try:
            until("storage readiness stopping", lambda: request("/readyz")[0] == 503, timeout=25)
            assert request("/healthz")[0] == 200
            status, _, html = request("/")
            assert status == 200 and b"storage-unavailable" in html
            assert request("/api/events/" + event, auth=auth)[0] == 503
        finally:
            command("docker", "start", STORE)
        until("storage and original incident recovery", lambda: request("/api/events/" + event, auth=auth)[0] == 200)
        assert api("/api/assets/" + lost_asset + "/samples/" + sample, auth=auth)["sample"]["metrics"]["collector"]["replicaId"] == other
        print(json.dumps({"failedLeader": original_leader, "survivor": other, "maintenanceFailoverSeconds": failover,
                          "leaseRecoveredSample": sample, "oneSampleInLeaseSlot": True, "eventId": event,
                          "deliveryId": delivery, "deliveryAttempts": 2, "storageDegradedAndRecovered": True}), flush=True)
    finally:
        ENV.write_text(original)
        webhook.shutdown()
        slow.shutdown()
        webhook.server_close()
        slow.server_close()
    command("bash", ARCHIVE)


if __name__ == "__main__":
    test()
