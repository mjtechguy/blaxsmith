#!/usr/bin/env python3
"""Scan synthetic probe surfaces for a token without printing the token."""

import json
import pathlib
import subprocess
import sys

if len(sys.argv) != 3:
    sys.exit("usage: scan-private-git.py TOKEN_FILE EVIDENCE_DIRECTORY")
token = pathlib.Path(sys.argv[1]).read_bytes()
evidence = pathlib.Path(sys.argv[2])
if len(token) < 24:
    sys.exit("synthetic token is too short")


def run(*args):
    return subprocess.run(args, check=True, capture_output=True).stdout


surfaces = {}
files = [path for path in evidence.rglob("*") if path.is_file()]
surfaces["evidence_files"] = {"checked": len(files),
    "token_found": any(token in path.read_bytes() for path in files)}

keys = run("kubectl", "-n", "ax-system", "exec", "deployment/ax-redis", "--",
    "redis-cli", "--raw", "--scan").decode().splitlines()
redis_dump = run("kubectl", "-n", "ax-system", "exec", "deployment/ax-redis", "--",
    "sh", "-c", 'redis-cli --scan | while IFS= read -r key; do redis-cli --raw DUMP "$key"; done')
surfaces["ax_redis"] = {"checked": len(keys), "token_found": token in redis_dump}

pod_list = json.loads(run("kubectl", "get", "pods", "-A", "-o", "json"))
prefixes = ("ax-controller", "ax-server", "blaxsmith-smoke", "atenet-router",
    "atenet-egress", "atelet")
logs_found = False
containers = 0
for item in pod_list["items"]:
    name = item["metadata"]["name"]
    if not name.startswith(prefixes):
        continue
    namespace = item["metadata"]["namespace"]
    for container in item["spec"]["containers"]:
        output = run("kubectl", "-n", namespace, "logs", "pod/" + name,
            "-c", container["name"], "--since=30m", "--tail=10000")
        containers += 1
        logs_found |= token in output
surfaces["runtime_logs"] = {"checked": containers, "token_found": logs_found}

database = run("pg_dump", "-d", "blaxsmith_dev", "--data-only")
surfaces["bootstrap_database"] = {"checked": 1, "token_found": token in database}
fixture_logs = run("journalctl", "-u", "blaxsmith-private-git-fixture", "--since", "30 minutes ago", "--no-pager")
surfaces["git_fixture_logs"] = {"checked": 1, "token_found": token in fixture_logs}

report = {"synthetic_secret_absent": not any(item["token_found"] for item in surfaces.values()),
    "surfaces": surfaces}
print(json.dumps(report, indent=2))
if not report["synthetic_secret_absent"]:
    sys.exit(1)
