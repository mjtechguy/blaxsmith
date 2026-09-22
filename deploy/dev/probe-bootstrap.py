#!/usr/bin/env python3
"""Synthetic AX bootstrap gate probe. No credential or private repository."""

import base64
import json
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

if len(sys.argv) != 4 or "@sha256:" not in sys.argv[1]:
    sys.exit("usage: probe-bootstrap.py PINNED_RUNNER_IMAGE ROUTER_URL NEW_EVIDENCE_DIRECTORY")

image, router, output = sys.argv[1], sys.argv[2].rstrip("/"), pathlib.Path(sys.argv[3])
output.mkdir()
space = "blaxsmith-gate-" + uuid.uuid4().hex[:10]
task = "gated-runner"
signer = Ed25519PrivateKey.generate()
public = signer.public_key().public_bytes(
    encoding=serialization.Encoding.Raw, format=serialization.PublicFormat.Raw
)
report = {"atespace": space, "image": image, "checks": []}


def ax(*args):
    result = subprocess.run(("ax", "-a", space, *args), text=True, capture_output=True, timeout=90)
    if result.returncode:
        raise RuntimeError(f"ax {' '.join(args)}: {result.stderr}")
    return result.stdout


def ate(*args):
    result = subprocess.run(("kubectl-ate", *args), text=True, capture_output=True, timeout=30)
    if result.returncode:
        raise RuntimeError(f"kubectl-ate {' '.join(args)}: {result.stderr}")
    return json.loads(result.stdout)


def wait_actor_state(state, seconds=90):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            actor = ate("get", "actor", task, "-a", space, "-o", "json")["actors"][0]
            if actor.get("status", {}).get("state") == state:
                return actor
        except (RuntimeError, IndexError, KeyError):
            pass
        time.sleep(2)
    raise RuntimeError(f"actor did not reach {state}")


def wait_template_ready():
    deadline = time.monotonic() + 240
    while time.monotonic() < deadline:
        try:
            actor = ate("get", "actor", task, "-a", space, "-o", "json")["actors"][0]
            template = actor["actorTemplate"]
            data = ate("get", "actor-template", template["name"], "-a", template["atespace"], "-o", "json")
            snapshot = data["actorTemplates"][0].get("status", {}).get("goldenSnapshotStatus", {}).get("goldenSnapshot")
            if snapshot:
                return
        except (RuntimeError, IndexError, KeyError):
            pass
        time.sleep(3)
    raise RuntimeError("golden snapshot did not become ready")


def activate_initial():
    deadline = time.monotonic() + 240
    last_resume = 0
    while time.monotonic() < deadline:
        try:
            actor = ate("get", "actor", task, "-a", space, "-o", "json")["actors"][0]
            state = actor.get("status", {}).get("state")
            if state == "ACTOR_STATE_RUNNING":
                return
            if state == "ACTOR_STATE_SUSPENDED" and time.monotonic() - last_resume > 10:
                template = actor["actorTemplate"]
                data = ate("get", "actor-template", template["name"], "-a", template["atespace"], "-o", "json")
                snapshot = data["actorTemplates"][0].get("status", {}).get("goldenSnapshotStatus", {}).get("goldenSnapshot")
                if snapshot:
                    ax("resume", "task", task)
                    last_resume = time.monotonic()
        except (RuntimeError, IndexError, KeyError):
            pass
        time.sleep(2)
    raise RuntimeError("initial actor did not run")


def request(path, body=None):
    headers = {"ate-target-actor": f"{space}/{task}"}
    if body is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(router + path, data=body, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def until_challenge():
    deadline = time.monotonic() + 150
    last = None
    while time.monotonic() < deadline:
        status, body = request("/blaxsmith/bootstrap/challenge")
        if status == 200:
            return json.loads(body)
        last = (status, body.decode(errors="replace")[:100])
        time.sleep(2)
    raise RuntimeError(f"challenge unavailable: {last}")


def signed_release(challenge):
    message = (
        "blaxsmith/bootstrap/release/v1\n"
        f"{challenge['nonce']}\n{challenge['expires_at']}\n"
        f"{challenge['atespace']}\n{challenge['task']}\n"
    ).encode()
    return json.dumps({
        "nonce": challenge["nonce"],
        "expires_at": challenge["expires_at"],
        "signature": base64.b64encode(signer.sign(message)).decode(),
    }).encode()


manifest = {
    "apiVersion": "ax.io/v1alpha1", "kind": "Task",
    "metadata": {"name": task, "atespace": space},
    "spec": {
        "image": image,
        "env": [{"name": "BLAXSMITH_BOOTSTRAP_PUBLIC_KEY", "value": base64.b64encode(public).decode()}],
        "command": ["/bin/sh", "-c", "echo released > /workspace/bootstrap-result"],
    },
}
(output / "task.json").write_text(json.dumps(manifest, indent=2) + "\n")

try:
    ax("apply", "-f", str(output / "task.json"))
    activate_initial()
    first = until_challenge()
    status, _ = request("/readyz")
    if status == 200:
        raise RuntimeError("workspace reported ready before release")
    report["checks"].append("workspace blocked before release")

    status, _ = request("/blaxsmith/bootstrap/release", signed_release(first))
    if status != 204:
        raise RuntimeError(f"initial release returned {status}")
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline and request("/readyz")[0] != 200:
        time.sleep(2)
    if request("/readyz")[0] != 200:
        raise RuntimeError("workspace did not become ready after release")
    report["checks"].append("signed release starts workspace")

    ax("suspend", "task", task)
    wait_actor_state("ACTOR_STATE_SUSPENDED")
    wait_template_ready()
    ax("resume", "task", task)
    wait_actor_state("ACTOR_STATE_RUNNING")
    second = until_challenge()
    if second["nonce"] == first["nonce"]:
        raise RuntimeError("resume reused pre-suspend challenge")
    status, _ = request("/readyz")
    if status == 200:
        raise RuntimeError("resumed workspace reported ready before fresh release")
    status, _ = request("/blaxsmith/bootstrap/release", signed_release(first))
    if status != 403:
        raise RuntimeError(f"old release on resume returned {status}")
    status, _ = request("/blaxsmith/bootstrap/release", signed_release(second))
    if status != 204:
        raise RuntimeError(f"fresh release on resume returned {status}")
    report["checks"].append("data-snapshot resume requires fresh release; replay denied")
    print("PASS bootstrap gate and resume replay probe", flush=True)
finally:
    try:
        ax("suspend", "task", task)
        wait_actor_state("ACTOR_STATE_SUSPENDED")
        report["final_phase"] = "Suspended"
    except Exception as error:
        report["cleanup_error"] = str(error)
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
print(output / "report.json")
