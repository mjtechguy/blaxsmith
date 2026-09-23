#!/usr/bin/env python3
"""Synthetic AX bootstrap gate probe. No credential or private repository."""

import base64
import http.client
import json
import os
import pathlib
import socket
import ssl
import subprocess
import sys
import time
import uuid

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
import yaml

if len(sys.argv) != 4 or "@sha256:" not in sys.argv[1] or not os.environ.get("BLAXSMITH_DEV_SIGNING_KEY_FILE"):
    sys.exit("usage: BLAXSMITH_DEV_SIGNING_KEY_FILE=... probe-bootstrap.py PINNED_RUNNER_IMAGE ROUTER_IP NEW_EVIDENCE_DIRECTORY")

image, router_ip, output = sys.argv[1], sys.argv[2], pathlib.Path(sys.argv[3])
router_host = "atenet-router.ate-system.svc"
output.mkdir()
space = "blaxsmith-gate-" + uuid.uuid4().hex[:10]
task = "gated-runner"
signer = Ed25519PrivateKey.from_private_bytes(pathlib.Path(os.environ["BLAXSMITH_DEV_SIGNING_KEY_FILE"]).read_bytes())
report = {"atespace": space, "image": image, "checks": []}
ledger_mode = os.environ.get("BLAXSMITH_DEV_LEDGER") == "1"
git_repo = os.environ.get("BLAXSMITH_DEV_GIT_REPO", "")
git_token_file = os.environ.get("BLAXSMITH_DEV_GIT_TOKEN_FILE", "")
if (git_repo == "") != (git_token_file == "") or (git_repo and not ledger_mode):
    sys.exit("private Git probe requires BLAXSMITH_DEV_LEDGER=1 and both Git inputs")


def kubectl(*args):
    result = subprocess.run(("kubectl", *args), text=True, capture_output=True, timeout=30)
    if result.returncode:
        raise RuntimeError(f"kubectl: {result.stderr}")
    return result.stdout


bundles = json.loads(kubectl("get", "clustertrustbundles.certificates.k8s.io", "-o", "json"))["items"]
matching = [item["spec"]["trustBundle"] for item in bundles
            if item["spec"]["signerName"] == "servicedns.podcert.ate.dev/identity"]
if len(matching) != 1:
    raise RuntimeError("expected exactly one service DNS trust bundle")
router_tls = ssl.create_default_context(cadata=matching[0])
connector_token = kubectl("-n", "ate-system", "create", "token", "blaxsmith-connector",
                          "--audience=blaxsmith-bootstrap", "--duration=10m").strip()
if ledger_mode:
    (output / "router-ca.pem").write_text(matching[0])
    (output / "actor-ca.pem").write_text(kubectl("-n", "ate-system", "exec", "deployment/atenet-egress",
        "-c", "ext-proc", "--", "cat", "/run/actor-id-ca-certs/ca.crt"))
    report["ledger_releases"] = []


class RouterConnection(http.client.HTTPSConnection):
    def connect(self):
        raw = socket.create_connection((router_ip, 443), timeout=15)
        self.sock = self._context.wrap_socket(raw, server_hostname=router_host)


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


def assert_data_snapshot(actor):
    snapshot = actor.get("status", {}).get("externalSnapshot", {})
    if snapshot.get("contentScope") != "SNAPSHOT_CONTENT_SCOPE_DATA" or not snapshot.get("snapshotUri"):
        raise RuntimeError("suspended actor lacks a data-only external snapshot")
    report["checks"].append("suspended actor persisted a data-only external snapshot")


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


def request(path, body=None, uid=None):
    headers = {"ate-target-actor": f"{space}/{task}"}
    if path.startswith("/blaxsmith/bootstrap/"):
        headers["Authorization"] = "Bearer " + connector_token
        headers["X-Blaxsmith-Actor-UID"] = current_uid if uid is None else uid
    if body is not None:
        headers["Content-Type"] = "application/json"
    connection = RouterConnection(router_host, context=router_tls, timeout=15)
    try:
        connection.request("POST" if body is not None else "GET", path, body=body, headers=headers)
        response = connection.getresponse()
        return response.status, response.read()
    finally:
        connection.close()


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


def release(challenge, previous_generation):
    if not ledger_mode:
        return request("/blaxsmith/bootstrap/release", signed_release(challenge))[0]
    args = ["go", "run", "./deploy/dev/ledger-release",
        "-space", space, "-task", task, "-image", image, "-pool", "blaxsmith-smoke",
        "-router-ip", router_ip, "-router-ca", str(output / "router-ca.pem"),
        "-actor-ca", str(output / "actor-ca.pem"),
        "-signer", os.environ["BLAXSMITH_DEV_SIGNING_KEY_FILE"],
        "-previous-generation", str(previous_generation)]
    if git_repo:
        args += ["-git-repo", git_repo, "-git-token-file", git_token_file]
    result = subprocess.run(args, input=connector_token, text=True, capture_output=True, timeout=90)
    if result.returncode:
        raise RuntimeError(f"ledger connector: {result.stderr.strip()}")
    record = json.loads(result.stdout)
    report["ledger_releases"].append(record)
    return 204


manifest = {
    "apiVersion": "ax.io/v1alpha1", "kind": "Task",
    "metadata": {"name": task, "atespace": space},
    "spec": {
        "image": image,
        "command": ["/bin/sh", "-c", "echo released > /workspace/bootstrap-result"],
    },
}
if git_repo:
    manifest["spec"]["debug"] = True
    manifest["spec"]["workspaces"] = [{"name": "private-source", "path": "/workspace"}]
    manifest["spec"]["gateway"] = {"name": "private-git-gateway"}
    manifest["spec"]["command"] = ["/bin/sh", "-c",
        "test -f /workspace/private/README.md && echo released > /workspace/bootstrap-result"]
    workspace = {"apiVersion": "ax.io/v1alpha1", "kind": "Workspace",
        "metadata": {"name": "private-source", "atespace": space},
        "spec": {"git": [{"name": "private", "repo": git_repo, "branch": "main", "dir": "private"}]}}
    gateway = {"apiVersion": "ax.io/v1alpha1", "kind": "Gateway",
        "metadata": {"name": "private-git-gateway", "atespace": space},
        "spec": {"egress": {"allowlist": {"hosts": [{"host": "10.42.0.1/32"}]}}}}
    (output / "workspace.json").write_text(json.dumps(workspace, indent=2) + "\n")
    (output / "gateway.json").write_text(json.dumps(gateway, indent=2) + "\n")
(output / "task.json").write_text(json.dumps(manifest, indent=2) + "\n")


def denied_task(name, spec, reason):
    rejected = {"apiVersion": "ax.io/v1alpha1", "kind": "Task",
                "metadata": {"name": name, "atespace": space}, "spec": spec}
    task_file = output / (name + ".json")
    task_file.write_text(json.dumps(rejected, indent=2) + "\n")
    ax("apply", "-f", str(task_file))
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        status = yaml.safe_load(ax("get", "task", name)).get("status", {})
        if status.get("phase") == "Failed":
            if not any(c.get("reason") == reason for c in status.get("conditions", [])):
                raise RuntimeError(f"{name} failed for a reason other than {reason}")
            report["checks"].append(name + " blocked before actor launch")
            return
        time.sleep(1)
    raise RuntimeError(f"{name} did not fail closed")

try:
    if git_repo:
        ax("apply", "-f", str(output / "workspace.json"))
        ax("apply", "-f", str(output / "gateway.json"))
    denied_task("task-signer-override", {"image": image,
        "env": [{"name": "BLAXSMITH_BOOTSTRAP_PUBLIC_KEY", "value": base64.b64encode(bytes(32)).decode()}],
        "command": ["touch", "/workspace/override-ran"]}, "BootstrapKeyOverride")
    denied_task("unapproved-runner", {"image": image[:-1] + ("0" if image[-1] != "0" else "1"),
        "command": ["touch", "/workspace/unapproved-ran"]}, "RunnerImageDenied")
    ax("apply", "-f", str(output / "task.json"))
    activate_initial()
    current_uid = ate("get", "actor", task, "-a", space, "-o", "json")["actors"][0]["metadata"]["uid"]
    first = until_challenge()
    if request("/blaxsmith/bootstrap/challenge", uid="stale-actor-uid")[0] != 409:
        raise RuntimeError("stale actor UID was routed to the guest")
    report["checks"].append("stale actor UID denied before challenge")
    status, _ = request("/readyz")
    if status == 200:
        raise RuntimeError("workspace reported ready before release")
    report["checks"].append("workspace blocked before release")

    status = release(first, 0)
    if status != 204:
        raise RuntimeError(f"initial release returned {status}")
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline and request("/readyz")[0] != 200:
        time.sleep(2)
    if request("/readyz")[0] != 200:
        raise RuntimeError("workspace did not become ready after release")
    report["checks"].append("signed release starts workspace")
    if git_repo:
        config = ax("ssh", task, "--", "/bin/sh", "-c",
            "test -f /workspace/private/README.md && test -f /workspace/bootstrap-result && cat /workspace/private/.git/config; for f in /ax/git-success.log /ax/git-error.log; do test ! -f \"$f\" || cat \"$f\"; done")
        token = pathlib.Path(git_token_file).read_text()
        if token in config or token in ax("ssh", task, "--", "/usr/bin/env"):
            raise RuntimeError("Git token entered workspace config or command environment")
        actor_data = ate("get", "actor", task, "-a", space, "-o", "json")
        template = actor_data["actors"][0]["actorTemplate"]
        template_data = ate("get", "actor-template", template["name"],
            "-a", template["atespace"], "-o", "json")
        for body in (ax("get", "task", task, "-o", "json"),
                     ax("get", "workspace", "private-source", "-o", "json"),
                     json.dumps(actor_data), json.dumps(template_data)):
            if token in body:
                raise RuntimeError("Git token entered control-plane object")
        report["checks"].append("private Git checkout succeeded without token in Git config or command environment")

    ax("suspend", "task", task)
    assert_data_snapshot(wait_actor_state("ACTOR_STATE_SUSPENDED"))
    wait_template_ready()
    ax("resume", "task", task)
    wait_actor_state("ACTOR_STATE_RUNNING")
    current_uid = ate("get", "actor", task, "-a", space, "-o", "json")["actors"][0]["metadata"]["uid"]
    second = until_challenge()
    if second["nonce"] == first["nonce"]:
        raise RuntimeError("resume reused pre-suspend challenge")
    status, _ = request("/readyz")
    if status == 200:
        raise RuntimeError("resumed workspace reported ready before fresh release")
    status, _ = request("/blaxsmith/bootstrap/release", signed_release(first))
    if status != 403:
        raise RuntimeError(f"old release on resume returned {status}")
    status = release(second, 1)
    if status != 204:
        raise RuntimeError(f"fresh release on resume returned {status}")
    report["checks"].append("data-snapshot resume requires fresh release; replay denied")
    if git_repo:
        resumed = None
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            result = subprocess.run(("ax", "-a", space, "ssh", task, "--", "/bin/sh", "-c",
                "test -f /workspace/private/README.md && cat /workspace/private/.git/config"),
                text=True, capture_output=True, timeout=20)
            if result.returncode == 0:
                resumed = result.stdout
                break
            time.sleep(2)
        if resumed is None:
            raise RuntimeError("resumed private workspace could not be inspected")
        if pathlib.Path(git_token_file).read_text() in resumed:
            raise RuntimeError("Git token appeared in resumed workspace")
        report["checks"].append("private checkout survived data-snapshot resume without token in Git config")
    print("PASS bootstrap gate and resume replay probe", flush=True)
finally:
    try:
        ax("suspend", "task", task)
        assert_data_snapshot(wait_actor_state("ACTOR_STATE_SUSPENDED"))
        report["final_phase"] = "Suspended"
    except Exception as error:
        report["cleanup_error"] = str(error)
    if ledger_mode and report["ledger_releases"]:
        last = report["ledger_releases"][-1]
        result = subprocess.run(["go", "run", "./deploy/dev/ledger-release", "-finish",
            "-space", space, "-task", task, "-previous-generation", str(last["owner_generation"])],
            text=True, capture_output=True, timeout=30)
        if result.returncode:
            report["owner_cleanup_error"] = result.stderr.strip()
        else:
            report["owner_cleanup"] = json.loads(result.stdout)
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
print(output / "report.json")
