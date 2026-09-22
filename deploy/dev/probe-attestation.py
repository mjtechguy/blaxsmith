#!/usr/bin/env python3
"""Verify a synthetic actor proof through Substrate ingress; no credentials."""

import base64
import json
import os
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.request

if len(sys.argv) != 5:
    sys.exit("usage: probe-attestation.py ATESPACE TASK ROUTER_URL NEW_EVIDENCE_DIRECTORY")
space, task, router, output = sys.argv[1], sys.argv[2], sys.argv[3].rstrip("/"), pathlib.Path(sys.argv[4])
output.mkdir()
report = {"atespace": space, "task": task, "checks": []}


def run(*args):
    result = subprocess.run(args, text=True, capture_output=True, timeout=60)
    if result.returncode:
        raise RuntimeError(f"{args[0]}: {result.stderr}")
    return result.stdout


def actor():
    return json.loads(run("kubectl-ate", "get", "actor", task, "-a", space, "-o", "json"))["actors"][0]


def wait_state(state):
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        current = actor()
        if current.get("status", {}).get("state") == state:
            return current
        time.sleep(2)
    raise RuntimeError(f"actor did not reach {state}")


def request(nonce, target=task):
    req = urllib.request.Request(router + "/blaxsmith/bootstrap/challenge", headers={
        "ate-target-actor": f"{space}/{target}",
        "X-Blaxsmith-Request-Nonce": nonce,
    })
    try:
        with urllib.request.urlopen(req, timeout=15) as response:
            return response.status, response.headers, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.headers, error.read()


try:
    if actor().get("status", {}).get("state") != "ACTOR_STATE_RUNNING":
        run("ax", "-a", space, "resume", "task", task)
    before = wait_state("ACTOR_STATE_RUNNING")
    uid = before["metadata"]["uid"]
    nonce = base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()
    status, headers, body = request(nonce)
    if status != 200 or not headers.get("X-Blaxsmith-Actor-Certificate") or not headers.get("X-Blaxsmith-Actor-Signature"):
        raise RuntimeError(f"actor attestation unavailable: HTTP {status}")
    if actor()["metadata"]["uid"] != uid:
        raise RuntimeError("actor replaced during challenge")
    proof = {
        "Atespace": space, "ActorName": task, "ActorUID": uid,
        "RequestNonce": nonce, "Body": base64.b64encode(body).decode(),
        "Certificate": headers["X-Blaxsmith-Actor-Certificate"],
        "Signature": headers["X-Blaxsmith-Actor-Signature"],
    }
    (output / "proof.json").write_text(json.dumps(proof, indent=2) + "\n")
    ca = run("kubectl", "-n", "ate-system", "exec", "deployment/atenet-egress", "-c", "ext-proc", "--",
             "cat", "/run/actor-id-ca-certs/ca.crt")
    (output / "actor-ca.pem").write_text(ca)
    checked = json.loads(run("go", "run", "./deploy/dev/verify-actor-proof.go", "-proof", str(output / "proof.json"),
                             "-ca", str(output / "actor-ca.pem")))
    report["actor_uid"] = uid
    report["checks"].extend(checked["checks"])
    status, _, _ = request("bad")
    if status != 400:
        raise RuntimeError(f"malformed nonce returned HTTP {status}")
    report["checks"].append("malformed connector nonce rejected by atunnel")
    status, _, _ = request(nonce, "other-actor")
    if status not in (404, 421):
        raise RuntimeError(f"wrong actor route returned HTTP {status}")
    report["checks"].append("wrong actor route rejected")
    print("PASS actor attestation and verifier negatives", flush=True)
finally:
    try:
        run("ax", "-a", space, "suspend", "task", task)
        wait_state("ACTOR_STATE_SUSPENDED")
        report["final_phase"] = "Suspended"
    except Exception as error:
        report["cleanup_error"] = str(error)
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
print(output / "report.json")
