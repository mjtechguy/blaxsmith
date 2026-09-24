#!/usr/bin/env python3
"""Verify a synthetic actor proof through Substrate ingress; no credentials."""

import base64
import functools
import http.client
import json
import os
import pathlib
import socket
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

if len(sys.argv) != 5:
    sys.exit("usage: probe-attestation.py ATESPACE TASK ROUTER_IP NEW_EVIDENCE_DIRECTORY")
space, task, router_ip, output = sys.argv[1], sys.argv[2], sys.argv[3], pathlib.Path(sys.argv[4])
router_host = "atenet-router.ate-system.svc"
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


@functools.cache
def service_ca():
    bundles = json.loads(run("kubectl", "get", "clustertrustbundles.certificates.k8s.io", "-o", "json"))["items"]
    matching = [item["spec"]["trustBundle"] for item in bundles
                if item["spec"]["signerName"] == "servicedns.podcert.ate.dev/identity"]
    if len(matching) != 1:
        raise RuntimeError("expected exactly one service DNS trust bundle")
    return matching[0]


class RouterConnection(http.client.HTTPSConnection):
    def connect(self):
        raw = socket.create_connection((router_ip, 443), timeout=15)
        self.sock = self._context.wrap_socket(raw, server_hostname=router_host)


def request(nonce, token=None, target=task, uid=None):
    headers = {"ate-target-actor": f"{space}/{target}", "X-Blaxsmith-Request-Nonce": nonce,
               "X-Blaxsmith-Actor-UID": uid if uid is not None else expected_uid}
    if token is not None:
        headers["Authorization"] = "Bearer " + token
    connection = RouterConnection(router_host, context=ssl.create_default_context(cadata=service_ca()), timeout=15)
    try:
        connection.request("GET", "/blaxsmith/bootstrap/challenge?phase=setup", headers=headers)
        response = connection.getresponse()
        return response.status, response.headers, response.read()
    finally:
        connection.close()


try:
    expected_uid = actor()["metadata"]["uid"]
    connector_token = run("kubectl", "-n", "ate-system", "create", "token", "blaxsmith-connector",
                          "--audience=blaxsmith-bootstrap", "--duration=10m").strip()
    other_token = run("kubectl", "-n", "ate-system", "create", "token", "atenet-router",
                      "--audience=blaxsmith-bootstrap", "--duration=10m").strip()
    wrong_audience = run("kubectl", "-n", "ate-system", "create", "token", "blaxsmith-connector",
                         "--audience=another-audience", "--duration=10m").strip()
    nonce = base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()
    for label, token, expected_status in [("missing", None, 401), ("wrong principal", other_token, 403),
                                           ("wrong audience", wrong_audience, 403)]:
        status, _, _ = request(nonce, token)
        if status != expected_status:
            raise RuntimeError(f"{label} connector auth returned HTTP {status}")
        report["checks"].append(f"{label} connector identity rejected before actor resume")
    if actor().get("status", {}).get("state") != "ACTOR_STATE_SUSPENDED":
        raise RuntimeError("denied bootstrap request changed actor state")
    req = urllib.request.Request("http://" + router_ip + "/blaxsmith/bootstrap/challenge?phase=setup", headers={
        "ate-target-actor": f"{space}/{task}", "x-forwarded-proto": "https",
        "X-Blaxsmith-Request-Nonce": nonce,
    })
    try:
        urllib.request.urlopen(req, timeout=15)
        raise RuntimeError("plaintext bootstrap request accepted")
    except urllib.error.HTTPError as error:
        if error.code != 426:
            raise RuntimeError(f"plaintext bootstrap returned HTTP {error.code}")
    report["checks"].append("plaintext request with spoofed TLS header rejected")
    if actor().get("status", {}).get("state") != "ACTOR_STATE_RUNNING":
        run("ax", "-a", space, "resume", "task", task)
    before = wait_state("ACTOR_STATE_RUNNING")
    uid = before["metadata"]["uid"]
    nonce = base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()
    status, headers, body = request(nonce, connector_token)
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
    status, _, _ = request(nonce, connector_token, uid="stale-actor-uid")
    if status != 409:
        raise RuntimeError(f"stale actor UID returned HTTP {status}")
    report["checks"].append("stale actor UID denied before routing")
    status, _, _ = request("bad", connector_token)
    if status != 400:
        raise RuntimeError(f"malformed nonce returned HTTP {status}")
    report["checks"].append("malformed connector nonce rejected by atunnel")
    status, _, _ = request(nonce, connector_token, "other-actor")
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
