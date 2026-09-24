#!/usr/bin/env python3
"""Synthetic AX bootstrap gate probe. No credential or private repository."""

import base64
import hashlib
import http.client
import ipaddress
import json
import os
import pathlib
import re
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
report = {"atespace": space, "image": image, "debug_task": False, "checks": []}
ledger_mode = os.environ.get("BLAXSMITH_DEV_LEDGER") == "1"
git_repo = os.environ.get("BLAXSMITH_DEV_GIT_REPO", "")
git_commit = os.environ.get("BLAXSMITH_DEV_GIT_COMMIT", "")
git_token_file = os.environ.get("BLAXSMITH_DEV_GIT_TOKEN_FILE", "")
secret_key_file = os.environ.get("BLAXSMITH_DEV_SECRET_KEY_FILE", "")
model_token_file = os.environ.get("BLAXSMITH_DEV_MODEL_TOKEN_FILE", "")
model_repo = "https://github.com/octocat/Hello-World.git"
model_ref = "master"
model_commit = "7fd1a60b01f91b314f59955a4e4d4e80d8edf11d"
report["debug_task"] = bool(git_repo)
if (git_repo == "") != (git_token_file == "") or (git_repo == "") != (git_commit == "") or (git_repo and not ledger_mode):
    sys.exit("private Git probe requires BLAXSMITH_DEV_LEDGER=1 and repository, commit, and token inputs")
if git_repo and not secret_key_file:
    sys.exit("private Git probe requires BLAXSMITH_DEV_SECRET_KEY_FILE")
model_mode = bool(model_token_file)
if model_mode and (not ledger_mode or git_repo or not secret_key_file or
        not re.fullmatch(r"[0-9a-f]{40}", model_commit)):
    sys.exit("model phase probe requires the ledger, a secret key, and a separate public source")
if model_mode:
    report["model_source"] = {"repository_url": model_repo, "source_ref": model_ref, "commit": model_commit}
    report["model_probe"] = {"provider": "openai", "model": "gpt-6-luna", "api_requests": False,
        "fake_cli_sha256": hashlib.sha256(pathlib.Path(__file__).with_name("model-probe-cli").read_bytes()).hexdigest()}
if git_commit:
    report["git_commit"] = git_commit


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


def until_challenge(phase="setup"):
    deadline = time.monotonic() + 150
    last = None
    while time.monotonic() < deadline:
        status, body = request("/blaxsmith/bootstrap/challenge?phase=" + phase)
        if status == 200:
            challenge = json.loads(body)
            if challenge.get("phase") != phase:
                raise RuntimeError(f"{phase} probe received a different phase")
            return challenge
        last = (status, body.decode(errors="replace")[:100])
        time.sleep(2)
    raise RuntimeError(f"challenge unavailable: {last}")


def public_gateway(name):
    addresses = set()
    for hostname in ("github.com",):
        for result in socket.getaddrinfo(hostname, 443, socket.AF_INET, socket.SOCK_STREAM):
            address = ipaddress.ip_address(result[4][0])
            if not address.is_global:
                raise RuntimeError(f"{hostname} resolved to a non-public address")
            addresses.add(str(address) + "/32")
    # A cluster resolver can return another public A record than the host
    # resolver. Keep exact routes and let the operator add only addresses
    # observed in the dev cluster's egress log.
    extra = os.environ.get("BLAXSMITH_DEV_MODEL_GIT_EGRESS_IPS", "")
    for value in filter(None, (item.strip() for item in extra.split(","))):
        address = ipaddress.ip_address(value)
        if not isinstance(address, ipaddress.IPv4Address) or not address.is_global:
            raise RuntimeError("extra model Git egress entries must be public IPv4 addresses")
        addresses.add(str(address) + "/32")
    routes = sorted(addresses)
    if name == "model-gateway":
        report["model_probe"]["git_egress_routes"] = routes
    return {"apiVersion": "ax.io/v1alpha1", "kind": "Gateway",
        "metadata": {"name": name, "atespace": space},
        "spec": {"egress": {"allowlist": {"hosts": [{"host": host} for host in routes]}}}}


def wait_workspace_ready():
    deadline = time.monotonic() + 180
    last = {}
    while time.monotonic() < deadline:
        last = yaml.safe_load(ax("get", "task", task)) or {}
        status = last.get("status", {})
        if status.get("phase") == "Running" and any(
                condition.get("type") == "WorkspaceReady" and condition.get("status") == "True" and
                condition.get("reason") == "SetupComplete" for condition in status.get("conditions", [])):
            report["checks"].append("AX reports WorkspaceReady=True/SetupComplete before model release")
            return
        time.sleep(2)
    raise RuntimeError("AX WorkspaceReady=True/SetupComplete was not observed: " + str(last.get("status", {}))[:500])


def wait_model_probe():
    expected = "synthetic model phase credential check passed"
    deadline = time.monotonic() + 90
    last = ""
    while time.monotonic() < deadline:
        result = subprocess.run(("ax", "-a", space, "ssh", task, "--", "/bin/sh", "-c",
            "cat /run/blaxsmith/model-probe-result; test ! -e /run/blaxsmith/agent-credential.json"),
            text=True, capture_output=True, timeout=25)
        last = result.stdout.strip()
        if result.returncode == 0 and last == expected:
            report["checks"].append("pinned fake CLI saw a 0600 model credential; runner removed it after exit")
            return
        time.sleep(2)
    raise RuntimeError("synthetic model worker did not finish cleanly: " + last[:160])


def clear_model_probe():
    ax("ssh", task, "--", "/bin/sh", "-c",
        "test ! -e /run/blaxsmith/agent-credential.json && rm -f /run/blaxsmith/model-probe-result")


def signed_release(challenge):
    phase = challenge["phase"]
    message = (
        "blaxsmith/bootstrap/release/v3\n"
        f"{phase}\n"
        f"{challenge['nonce']}\n{challenge['expires_at']}\n"
        f"{challenge['atespace']}\n{challenge['task']}\n"
    ).encode()
    return json.dumps({
        "phase": phase,
        "nonce": challenge["nonce"],
        "expires_at": challenge["expires_at"],
        "signature": base64.b64encode(signer.sign(message)).decode(),
    }).encode()


def release(challenge, previous_generation):
    phase = challenge["phase"]
    if not ledger_mode:
        return request("/blaxsmith/bootstrap/release", signed_release(challenge))[0]
    args = ["go", "run", "./deploy/dev/ledger-release",
        "-space", space, "-task", task, "-image", image, "-pool", "blaxsmith-smoke",
        "-router-ip", router_ip, "-router-ca", str(output / "router-ca.pem"),
        "-actor-ca", str(output / "actor-ca.pem"),
        "-signer", os.environ["BLAXSMITH_DEV_SIGNING_KEY_FILE"],
        "-previous-generation", str(previous_generation), "-phase", phase]
    if git_repo:
        args += ["-git-repo", git_repo, "-git-commit", git_commit,
                 "-git-binding", report["access_seed"]["binding_id"], "-secret-key-file", secret_key_file]
    if model_mode:
        args += ["-model-binding", report["access_seed"]["model_binding_id"],
                 "-model-provider", "openai", "-model", "gpt-6-luna",
                 "-secret-key-file", secret_key_file]
    result = subprocess.run(args, input=connector_token, text=True, capture_output=True, timeout=90)
    if result.returncode:
        raise RuntimeError(f"ledger connector: {result.stderr.strip()}")
    record = json.loads(result.stdout)
    report["ledger_releases"].append(record)
    if (git_repo and phase == "setup") or (model_mode and phase == "model"):
        lease_id = record["access_lease"]
        if not re.fullmatch(r"[A-Za-z0-9_-]{22}", lease_id):
            raise RuntimeError("invalid access lease ID")
        model_lease = phase == "model"
        capability = "model.invoke" if model_lease else "git.read"
        resource = "openai/gpt-6-luna" if model_lease else git_repo
        secret_version = report["access_seed"]["model_secret_version"] if model_lease else report["access_seed"]["secret_version"]
        query = ("SELECT capability,resource,owner_generation,actor_uid,secret_version,"
                 "delivery_attempted_at IS NOT NULL,delivered_at IS NOT NULL,revoked_at IS NOT NULL "
                 "FROM access_leases WHERE organization_id='synthetic-org' AND id='" + lease_id + "'")
        state = subprocess.run(("psql", "-d", "blaxsmith_dev", "-Atc", query),
                               text=True, capture_output=True, timeout=15)
        expected = f"{capability}|{resource}|{record['owner_generation']}|{record['actor_uid']}|{secret_version}|t|t|f"
        if state.returncode or state.stdout.strip() != expected:
            raise RuntimeError("access lease is not a committed delivery for the current owner")
        report.setdefault("access_leases", []).append({"id": lease_id,
            "capability": capability, "resource": resource,
            "owner_generation": record["owner_generation"], "actor_uid": record["actor_uid"],
            "secret_version": secret_version, "delivered": True})
    return 204


manifest = {
    "apiVersion": "ax.io/v1alpha1", "kind": "Task",
    "metadata": {"name": task, "atespace": space},
    "spec": {
        "image": image,
        "resources": {
            "requests": {"cpu": "1", "memory": "1Gi"},
            "limits": {"cpu": "1", "memory": "1Gi"},
        },
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
elif model_mode:
    cli_path = pathlib.Path(__file__).with_name("model-probe-cli")
    request_body = {
        "attempt_id": space + "/" + task,
        "repository_url": model_repo,
        "source_ref": model_ref,
        "source_commit": model_commit,
        "source_directory": "source",
        "runtime": {"Harness": "codex", "Image": image,
            "Binary": "/usr/local/bin/blaxsmith-model-probe-cli",
            "BinarySHA256": hashlib.sha256(cli_path.read_bytes()).hexdigest(),
            "Version": "0.156.1", "Supported": [{"Model": "gpt-6-luna", "Effort": "xhigh"}]},
        "profile": {"harness": "codex", "model": "gpt-6-luna", "effort": "xhigh"},
        "prompt": "Synthetic local credential-path probe. Do not make provider requests.",
        "timeout_seconds": 60, "max_output_bytes": 2048,
    }
    manifest["spec"]["workspaces"] = [{"name": "model-source", "path": "/workspace"}]
    manifest["spec"]["gateway"] = {"name": "model-gateway"}
    manifest["spec"]["command"] = ["/usr/local/bin/blaxsmith-tool-worker",
        json.dumps(request_body, separators=(",", ":"))]
    workspace = {"apiVersion": "ax.io/v1alpha1", "kind": "Workspace",
        "metadata": {"name": "model-source", "atespace": space},
        "spec": {"git": [{"name": "source", "repo": model_repo,
            "branch": model_commit, "dir": "source", "depth": 1}]}}
    (output / "workspace.json").write_text(json.dumps(workspace, indent=2) + "\n")
    (output / "gateway.json").write_text(json.dumps(public_gateway("model-gateway"), indent=2) + "\n")
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
    if git_repo or model_mode:
        seed_args = ["go", "run", "./deploy/dev/seed-access", "-space", space,
            "-task", task, "-key-file", secret_key_file]
        if git_repo:
            seed_args += ["-repo", git_repo, "-commit", git_commit, "-token-file", git_token_file]
        if model_mode:
            seed_args += ["-model-token-file", model_token_file]
        seed = subprocess.run(seed_args,
            text=True, capture_output=True, timeout=45)
        if seed.returncode:
            raise RuntimeError(f"synthetic access seed: {seed.stderr.strip()}")
        report["access_seed"] = json.loads(seed.stdout)
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
    if request("/blaxsmith/bootstrap/challenge?phase=setup", uid="stale-actor-uid")[0] != 409:
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
    if model_mode:
        wait_workspace_ready()
        clear_model_probe()
        model_challenge = until_challenge("model")
        if request("/readyz")[0] != 200:
            raise RuntimeError("model phase opened before setup readiness")
        status = release(model_challenge, report["ledger_releases"][-1]["owner_generation"])
        if status != 204:
            raise RuntimeError(f"model release returned {status}")
        wait_model_probe()
    task_resources = manifest["spec"]["resources"]
    actor = ate("get", "actor", task, "-a", space, "-o", "json")["actors"][0]
    actor_template = actor["actorTemplate"]
    template = ate("get", "actor-template", actor_template["name"],
        "-a", actor_template["atespace"], "-o", "json")["actorTemplates"][0]
    placement = {item["name"]: item["quantity"] for item in template.get("resources", {}).get("limits", [])}
    container = template.get("containers", [{}])[0]
    container_limits = {item["name"]: item["quantity"] for item in container.get("resources", {}).get("limits", [])}
    assignment = actor["status"]["workerAssignment"]
    worker = json.loads(kubectl("get", "pod", "-n", assignment["workerNamespace"],
        assignment["workerPod"], "-o", "json"))
    if worker["metadata"]["uid"] != assignment["workerPodUid"]:
        raise RuntimeError("actor moved to a different WorkerPod during resource inspection")
    container_id = next(item["containerID"].split("://", 1)[1]
        for item in worker["status"]["containerStatuses"] if item["name"] == "ateom")
    ateom = json.loads(subprocess.check_output(("crictl", "inspect", container_id),
        text=True, timeout=30))
    pid = ateom["info"]["pid"]
    cgroup_path = next(line[3:] for line in pathlib.Path(f"/proc/{pid}/cgroup").read_text().splitlines()
        if line.startswith("0::"))
    if not cgroup_path.endswith("/ateom"):
        raise RuntimeError(f"unexpected WorkerPod cgroup path: {cgroup_path}")
    sandbox_cgroup = pathlib.Path("/sys/fs/cgroup") / cgroup_path.lstrip("/")
    sandbox_cgroup = sandbox_cgroup.parent / "_pause"
    cgroup = [(sandbox_cgroup / name).read_text().strip() for name in ("cpu.max", "memory.max")]
    expected_cgroup = ["100000 100000", "1073741824"]
    if task_resources["requests"] != task_resources["limits"]:
        raise RuntimeError("probe requires equal AX requests and limits at this Substrate pin")
    if placement != task_resources["requests"] or container_limits != task_resources["limits"]:
        raise RuntimeError(f"Substrate template resource mapping differs: placement={placement} container={container_limits}")
    if cgroup != expected_cgroup:
        raise RuntimeError(f"worker sandbox cgroup limits = {cgroup}, want {expected_cgroup}")
    report["resources"] = {
        "task": task_resources,
        "substrate_placement": placement,
        "task_container_oci_limits": container_limits,
        "worker_pool": assignment["workerPool"],
        "worker_pod": assignment["workerPod"],
        "worker_pod_uid": assignment["workerPodUid"],
        "sandbox_cgroup": {
            "path": "/_pause",
            "read_source": "Node cgroup v2, resolved from the WorkerPod ateom process; guest does not mount cgroupfs.",
            "cpu.max": cgroup[0],
            "memory.max": cgroup[1],
        },
    }
    report["checks"].append("AX resources reached Substrate placement and gVisor sandbox cgroups")
    if git_repo:
        config = ax("ssh", task, "--", "/bin/sh", "-c",
            "test -f /workspace/private/README.md && test -f /workspace/bootstrap-result && cat /workspace/private/.git/config; for f in /ax/git-success.log /ax/git-error.log; do test ! -f \"$f\" || cat \"$f\"; done")
        checked_out = ax("ssh", task, "--", "/usr/bin/git", "-C", "/workspace/private", "rev-parse", "HEAD").strip()
        if checked_out != git_commit:
            raise RuntimeError("private checkout differs from pinned commit")
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
        report["checks"].append("private Git checkout matched pinned commit without token in Git config or command environment")

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
    if model_mode:
        wait_workspace_ready()
        clear_model_probe()
        resumed_model = until_challenge("model")
        if resumed_model["nonce"] == model_challenge["nonce"]:
            raise RuntimeError("resumed actor reused the previous model challenge")
        status = release(resumed_model, report["ledger_releases"][-1]["owner_generation"])
        if status != 204:
            raise RuntimeError(f"model release on resume returned {status}")
        wait_model_probe()
        report["checks"].append("data-snapshot resume requires a fresh post-ready model release")
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
        resumed_commit = ax("ssh", task, "--", "/usr/bin/git", "-C", "/workspace/private", "rev-parse", "HEAD").strip()
        if resumed_commit != git_commit:
            raise RuntimeError("resumed checkout differs from pinned commit")
        if pathlib.Path(git_token_file).read_text() in resumed:
            raise RuntimeError("Git token appeared in resumed workspace")
        report["checks"].append("pinned private checkout survived data-snapshot resume without token in Git config")
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
