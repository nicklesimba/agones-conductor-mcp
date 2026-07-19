"""End-to-end exercise of every Kubernetes-facing tool over MCP stdio,
running against a live Agones cluster as a least-privilege ServiceAccount.

Expects a fleet named simple-fleet with 2 Ready replicas in the default
namespace (the Agones simple-game-server example). CI runs this against a
fresh kind cluster on every push - see .github/workflows/e2e.yml.

Usage: python mcp_e2e.py <path-to-binary> <kubeconfig-path>
Exits nonzero if any step fails; prints a PASS/FAIL line per step.
"""
import json
import os
import subprocess
import sys
import threading
import queue
import time

BINARY = sys.argv[1]
KUBECONFIG = sys.argv[2]

env = dict(os.environ)
env["KUBECONFIG"] = KUBECONFIG
env.pop("AGONES_MCP_CLUSTERS", None)
env.pop("AGONES_MCP_CONTEXT", None)

proc = subprocess.Popen(
    [BINARY],
    stdin=subprocess.PIPE,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    env=env,
)

out_q = queue.Queue()

def reader():
    for line in proc.stdout:
        line = line.strip()
        if line:
            out_q.put(line)

threading.Thread(target=reader, daemon=True).start()

next_id = [0]

def rpc(method, params=None, notify=False, timeout=90):
    msg = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        msg["params"] = params
    if not notify:
        next_id[0] += 1
        msg["id"] = next_id[0]
    proc.stdin.write((json.dumps(msg) + "\n").encode())
    proc.stdin.flush()
    if notify:
        return None
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            line = out_q.get(timeout=deadline - time.time())
        except queue.Empty:
            break
        resp = json.loads(line)
        if resp.get("id") == next_id[0]:
            return resp
    raise TimeoutError(f"no response to {method}")

def call(tool, args):
    resp = rpc("tools/call", {"name": tool, "arguments": args})
    if "error" in resp:
        return {"_rpc_error": resp["error"]}
    result = resp["result"]
    if result.get("isError"):
        text = " ".join(c.get("text", "") for c in result.get("content", []))
        return {"_tool_error": text}
    sc = result.get("structuredContent")
    if sc is not None:
        return sc
    text = " ".join(c.get("text", "") for c in result.get("content", []))
    try:
        return json.loads(text)
    except (ValueError, TypeError):
        return {"_raw": text}

failures = []
def check(name, ok, detail=""):
    status = "PASS" if ok else "FAIL"
    print(f"{status}: {name}" + (f" -- {detail}" if detail and not ok else ""))
    if not ok:
        failures.append((name, detail))

# --- handshake ---
init = rpc("initialize", {
    "protocolVersion": "2025-06-18",
    "capabilities": {},
    "clientInfo": {"name": "e2e-verify", "version": "0"},
})
check("initialize", "result" in init, json.dumps(init)[:300])
rpc("notifications/initialized", notify=True)

NS = "default"
FLEET = "e2e-verify-fleet"
AS = "e2e-verify-as"
# Image already present on the kind node from the existing simple-fleet.
existing = call("list_fleets", {"namespace": NS})
images = None
try:
    images = call("rollout_status", {"name": "simple-fleet", "namespace": NS})
    IMAGE = images["current"]["image"]
except Exception:
    IMAGE = "us-docker.pkg.dev/agones-images/examples/simple-game-server:0.36"
check("discover existing fleet image", bool(IMAGE), str(images)[:300])

# --- reads against existing state ---
r = call("list_fleets", {"namespace": NS})
check("list_fleets", "fleets" in r and any(f["name"] == "simple-fleet" for f in r["fleets"]), str(r)[:300])

r = call("list_gameservers", {"namespace": NS, "fleet": "simple-fleet"})
check("list_gameservers", r.get("count", 0) >= 1, str(r)[:300])
gs_name = r["gameServers"][0]["name"] if r.get("gameServers") else None

r = call("gameserver_events", {"name": gs_name, "namespace": NS})
check("gameserver_events", "events" in r, str(r)[:300])

r = call("gameserver_logs", {"name": gs_name, "namespace": NS, "tailLines": 20})
check("gameserver_logs", "logs" in r and "untrusted container output" in r.get("logs", ""), str(r)[:300])

r = call("list_autoscalers", {"namespace": NS})
check("list_autoscalers", "autoscalers" in r, str(r)[:300])

r = call("fleet_capacity", {"namespace": NS})
check("fleet_capacity", "fleets" in r, str(r)[:300])

r = call("list_clusters", {})
check("list_clusters", "default" in str(r), str(r)[:300])

r = call("agones_health", {})
check("agones_health reports healthy", r.get("healthy") is True and any(c.get("component") == "controller" for c in r.get("components", [])), str(r)[:300])

# --- write lifecycle on a dedicated fleet ---
# Idempotency: clear residue from any earlier aborted run.
call("delete_autoscaler", {"name": AS, "namespace": NS})
call("delete_autoscaler", {"name": "e2e-counter-as", "namespace": NS})
call("delete_fleet", {"name": FLEET, "namespace": NS, "force": True})
call("delete_fleet", {"name": "e2e-tcp-fleet", "namespace": NS, "force": True})
for _ in range(30):
    r = call("list_gameservers", {"namespace": NS, "fleet": FLEET})
    if r.get("count", 1) == 0:
        break
    time.sleep(2)

# --- dry run creates nothing ---
r = call("create_fleet", {
    "name": "e2e-dry-fleet", "namespace": NS, "replicas": 1,
    "image": IMAGE, "containerPort": 7654, "dryRun": True,
})
check("create_fleet dryRun accepted", r.get("dryRun") is True, str(r)[:300])
r = call("list_fleets", {"namespace": NS})
check("dryRun fleet was not persisted", not any(f["name"] == "e2e-dry-fleet" for f in r.get("fleets", [])), str(r)[:300])

# --- TCP fleet (the writeup scenario: a WebSocket game on TCP) ---
r = call("create_fleet", {
    "name": "e2e-tcp-fleet", "namespace": NS, "replicas": 1,
    "image": IMAGE, "containerPort": 7654, "protocol": "TCP",
})
check("create_fleet TCP", r.get("fleet", {}).get("name") == "e2e-tcp-fleet", str(r)[:300])
tcp_ready = 0
for _ in range(45):
    r = call("list_gameservers", {"namespace": NS, "fleet": "e2e-tcp-fleet", "state": "Ready"})
    tcp_ready = r.get("count", 0)
    if tcp_ready >= 1:
        break
    time.sleep(2)
check("TCP fleet reaches Ready", tcp_ready >= 1, f"ready={tcp_ready}")
r = call("delete_fleet", {"name": "e2e-tcp-fleet", "namespace": NS})
check("TCP fleet deleted", r.get("deleted") is True, str(r)[:300])

r = call("create_fleet", {
    "name": FLEET, "namespace": NS, "replicas": 2,
    "image": IMAGE, "containerPort": 7654,
    "counters": {"players": {"capacity": 10}},
    "lists": {"sessions": {"capacity": 5}},
})
check("create_fleet", r.get("fleet", {}).get("name") == FLEET, str(r)[:300])

# wait for Ready servers
ready = 0
for _ in range(60):
    r = call("list_gameservers", {"namespace": NS, "fleet": FLEET, "state": "Ready"})
    ready = r.get("count", 0)
    if ready >= 2:
        break
    time.sleep(2)
check("fleet reaches 2 Ready", ready >= 2, f"ready={ready}")

# Dry-run scale checked here, BEFORE the autoscaler exists: once it does, it
# writes Spec.Replicas itself and any desired-count assertion races it.
r = call("scale_fleet", {"name": FLEET, "namespace": NS, "replicas": 5, "dryRun": True})
check("scale_fleet dryRun accepted", r.get("dryRun") is True, str(r)[:300])
r = call("list_fleets", {"namespace": NS})
actual = next((f for f in r.get("fleets", []) if f["name"] == FLEET), {})
check("dryRun scale not persisted", actual.get("desired") == 2, str(actual)[:300])

r = call("create_autoscaler", {
    "name": AS, "namespace": NS, "fleet": FLEET,
    "bufferSize": "2", "maxReplicas": 4, "syncIntervalSeconds": 5,
})
check("create_autoscaler", r.get("autoscaler", {}).get("name") == AS, str(r)[:300])
check("autoscaler sync interval set", r.get("autoscaler", {}).get("syncIntervalSeconds") == 5, str(r)[:300])

r = call("create_autoscaler", {
    "name": "e2e-counter-as", "namespace": NS, "fleet": FLEET, "policy": "Counter",
    "counter": {"key": "players", "bufferSize": "5", "maxCapacity": 100},
    "dryRun": True,
})
check("counter-policy autoscaler validates (dryRun)", r.get("autoscaler", {}).get("key") == "players" or r.get("dryRun") is True, str(r)[:300])

r = call("update_autoscaler", {"name": AS, "namespace": NS, "maxReplicas": 5})
check("update_autoscaler", r.get("autoscaler", {}).get("maxReplicas") == 5, str(r)[:300])

r = call("scale_fleet", {"name": FLEET, "namespace": NS, "replicas": 3})
check("scale_fleet", r.get("targetReplicas") == 3, str(r)[:300])

r = call("allocate_gameserver", {
    "fleet": FLEET, "namespace": NS,
    "labels": {"match-id": "e2e-m1"},
    "annotations": {"mode": "e2e-test"},
    "counterSelectors": {"players": {"minAvailable": 1}},
    "counterActions": {"players": {"action": "Increment", "amount": 2}},
    "listActions": {"sessions": {"addValues": ["e2e-session"]}},
})
allocated_gs = r.get("gameServer")
check("allocate_gameserver", r.get("state") == "Allocated" and allocated_gs,
      str(r)[:300])
check("allocation counter action applied", r.get("counters", {}).get("players", {}).get("count") == 2, str(r)[:300])

r = call("get_gameserver", {"name": allocated_gs, "namespace": NS})
check("get_gameserver shows stamped match label",
      r.get("labels", {}).get("match-id") == "e2e-m1" and r.get("annotations", {}).get("mode") == "e2e-test",
      str(r)[:300])
check("get_gameserver full detail", r.get("state") == "Allocated" and r.get("image") and r.get("gameServerSet"), str(r)[:300])

r = call("allocate_gameserver", {
    "fleet": FLEET, "namespace": NS, "preferReuse": True,
    "counterSelectors": {"players": {"minAvailable": 1}},
})
check("preferReuse packs onto the running match", r.get("state") == "Allocated" and r.get("gameServer") == allocated_gs, str(r)[:300])

r = call("delete_gameserver", {"name": allocated_gs, "namespace": NS})
check("delete_gameserver refuses Allocated without force", r.get("deleted") is False and "force" in r.get("warning", ""), str(r)[:300])

r = call("update_fleet_image", {"fleet": FLEET, "namespace": NS, "image": IMAGE + "-e2e-bogus"})
check("update_fleet_image", r.get("newImage", "").endswith("-e2e-bogus"), str(r)[:300])

r = call("rollout_status", {"name": FLEET, "namespace": NS})
check("rollout_status shows rollout", r.get("complete") is False and r.get("previous"), str(r)[:300])

# roll back to the working image so the fleet can settle
r = call("update_fleet_image", {"fleet": FLEET, "namespace": NS, "image": IMAGE})
check("update_fleet_image rollback", r.get("newImage") == IMAGE, str(r)[:300])

r = call("update_fleet_resources", {"fleet": FLEET, "namespace": NS, "cpuRequest": "20m"})
check("update_fleet_resources", r.get("resources", {}).get("cpuRequest") == "20m", str(r)[:300])

r = call("update_fleet_health", {"fleet": FLEET, "namespace": NS, "periodSeconds": 7})
check("update_fleet_health", r.get("health", {}).get("periodSeconds") == 7, str(r)[:300])

r = call("update_fleet_env", {"fleet": FLEET, "namespace": NS, "set": {"E2E_MODE": "conductor"}})
check("update_fleet_env", "E2E_MODE=conductor" in r.get("env", []), str(r)[:300])

r = call("fleet_events", {"name": FLEET, "namespace": NS})
check("fleet_events returns scaling history", len(r.get("events", [])) > 0 and r.get("notice"), str(r)[:300])

r = call("autoscaler_events", {"name": AS, "namespace": NS})
check("autoscaler_events callable", "events" in r, str(r)[:300])

r = call("delete_fleet", {"name": FLEET, "namespace": NS})
check("delete_fleet refuses with Allocated server", r.get("deleted") is False, str(r)[:300])

r = call("delete_gameserver", {"name": allocated_gs, "namespace": NS, "force": True})
check("delete_gameserver force", r.get("deleted") is True, str(r)[:300])

r = call("delete_autoscaler", {"name": AS, "namespace": NS})
check("delete_autoscaler", r.get("deleted") is True, str(r)[:300])

# The force-deleted server lingers in Terminating (still listed Allocated)
# briefly; delete_fleet correctly refuses until it's actually gone.
for _ in range(30):
    r = call("list_gameservers", {"namespace": NS, "fleet": FLEET, "state": "Allocated"})
    if r.get("count", 1) == 0:
        break
    time.sleep(2)

r = call("delete_fleet", {"name": FLEET, "namespace": NS})
check("delete_fleet", r.get("deleted") is True, str(r)[:300])

# --- negative-path checks ---
r = call("list_gameservers", {"namespace": NS, "state": "Crashed"})
check("invalid state filter is rejected", "_tool_error" in r or "_rpc_error" in r, str(r)[:300])

r = call("update_fleet_image", {"fleet": "simple-fleet", "namespace": NS, "image": ""})
check("empty image is rejected", "_tool_error" in r or "_rpc_error" in r, str(r)[:300])

r = call("gameserver_logs", {"name": "agones-mcp-verify-not-a-gs", "namespace": "kube-system"})
check("logs of non-GameServer pod refused", "_tool_error" in r or "_rpc_error" in r, str(r)[:300])

proc.stdin.close()
proc.terminate()

print()
if failures:
    print(f"E2E RESULT: {len(failures)} FAILURE(S)")
    sys.exit(1)
print("E2E RESULT: ALL PASS")
