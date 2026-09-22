#!/usr/bin/env python3
"""Synthetic launch checks on the dedicated AX node. No provider credentials.
Requires explicit KUBECONFIG plus patched controller and a free gVisor worker.
Creates a unique atespace, retains evidence, and leaves the task suspended.
"""
import json
import pathlib
import re
import shlex
import subprocess
import sys
import time
import uuid

if len(sys.argv) != 3 or "@sha256:" not in sys.argv[1]:
    sys.exit("usage: probe-launch.py PINNED_RUNNER_IMAGE NEW_EVIDENCE_DIRECTORY")
image, output = sys.argv[1], pathlib.Path(sys.argv[2])
output.mkdir()
space = "blaxsmith-launch-" + uuid.uuid4().hex[:10]
checks = []
report = {"atespace": space, "runner_image": image, "checks": checks}


def run(*args, check=True, timeout=45):
    result = subprocess.run(args, text=True, capture_output=True, timeout=timeout)
    if check and result.returncode:
        raise RuntimeError(f"{args[0]} failed: {result.stderr}")
    return result


def ax(*args, **kwargs):
    return run("ax", "-a", space, *args, **kwargs)


def apply(kind, name, spec):
    manifest = {"apiVersion": "ax.io/v1alpha1", "kind": kind,
                "metadata": {"name": name, "atespace": space}, "spec": spec}
    path = output / f"{kind}-{name}.json"
    path.write_text(json.dumps(manifest, indent=2) + "\n")
    ax("apply", "-f", str(path))


def wait_for(name, phase, reason="", seconds=30):
    deadline, retries, next_retry = time.monotonic() + seconds, 0, 0
    while time.monotonic() < deadline:
        description = ax("describe", "task", name).stdout
        if re.search(r"^Phase:\s+" + phase + r"\s*$", description, re.M) and reason in description:
            (output / f"{name}-{phase}.txt").write_text(description)
            return
        if phase == "Running" and "ResourceExhausted" in description:
            if retries < 3 and time.monotonic() >= next_retry:
                retries, next_retry = retries + 1, time.monotonic() + 20
                print(f"Waiting for template capacity; retry {retries}/3", flush=True)
                ax("resume", "task", name)
        time.sleep(2)
    raise RuntimeError(f"{name} did not reach {phase} / {reason}: {description}")


def guest(script):
    return ax("ssh", "runner", "--", "/bin/sh", "-c", script)


try:
    for name, spec, reason in [
        ("missing-gateway", {"gateway": {"name": "absent"}}, "GatewayUnavailable"),
        ("missing-workspace", {"workspaces": [{"name": "absent"}]}, "WorkspaceUnavailable"),
    ]:
        spec.update(image=image, command=["touch", "/workspace/should-not-start"])
        apply("Task", name, spec)
        wait_for(name, "Failed", reason)
        actor = run("kubectl-ate", "get", "actor", name, "-a", space, check=False)
        if actor.returncode == 0 or "NotFound" not in actor.stderr:
            raise RuntimeError("Expected no actor: " + actor.stdout + actor.stderr)
        checks.append({"check": name, "result": "blocked-before-actor"})
        print(f"PASS {name}: {reason}", flush=True)

    apply("Gateway", "network", {"egress": {"allowlist": {"hosts": [{"host": "*"}]}}})
    runner_spec = {"image": image, "debug": True, "gateway": {"name": "network"},
                   "command": ["/bin/sh", "-c", "echo blaxsmith-launch-ok > /workspace/launch-result"]}
    apply("Task", "runner", runner_spec)
    wait_for("runner", "Running", "SetupComplete", seconds=150)
    guest('test "$(cat /workspace/launch-result)" = blaxsmith-launch-ok')
    checks.append({"check": "valid-command", "result": "passed"})

    for kind in ["missing", "directory", "git", "skills", "goal"]:
        root = "/tmp/blaxsmith-" + uuid.uuid4().hex
        ws = {"metadata": {"name": "required"}, "spec": {}}
        ref = {"name": "required", "path": root + "/workspace"}
        if kind == "directory":
            ref["path"] = root + "/file"
        if kind == "git":
            ws["spec"]["git"] = [{"repo": root + "/missing.git"}]
        if kind == "skills":
            ws["spec"]["skills"] = {"path": root + "/file"}
        if kind == "goal":
            ref["goal"] = "required bootstrap is unavailable"
        task = {"metadata": {"name": "nested-probe"}, "spec": {
            "workspaces": [ref], "command": ["touch", root + "/command-started"]}}
        script = f"""set -eu
mkdir -p {root}
printf x > {root}/file
printf %s {shlex.quote(json.dumps(task))} > {root}/task.json
printf %s {shlex.quote(json.dumps(ws))} > {root}/workspace.json
unset GEMINI_API_KEY AX_WORKSPACES_YAML
if /usr/local/bin/ax-task-runner --port 18080 --task-file {root}/task.json {'' if kind == 'missing' else '--workspace-file ' + root + '/workspace.json'} > {root}/runner.log 2>&1; then
  echo 'FAIL: incomplete input accepted'; exit 1
fi
test ! -e {root}/command-started
echo 'PASS incomplete {kind}'
"""
        (output / f"runner-{kind}.txt").write_text(guest(script).stdout)
        checks.append({"check": "runner-" + kind, "result": "command-not-started"})
        print(f"PASS runner rejects {kind}", flush=True)

    proof = uuid.uuid4().hex
    guest(f"printf %s {proof} > /workspace/launch-resume-proof")
    ax("suspend", "task", "runner")
    wait_for("runner", "Suspended")
    ax("resume", "task", "runner")
    wait_for("runner", "Running", "SetupComplete", seconds=60)
    guest(f'test "$(cat /workspace/launch-resume-proof)" = {proof}')
    checks.append({"check": "resume-file-persistence", "result": "passed"})
    print("PASS suspend/resume persistence", flush=True)

    # Probe the same reachable endpoint through four policy revisions. Each
    # wget opens a new CONNECT; existing tunnels are outside this check.
    service_ip = run("kubectl", "-n", "blaxsmith-build", "get", "svc", "registry",
                     "-o", "jsonpath={.spec.clusterIP}").stdout.strip()
    probe = f"wget -T 5 -qO /dev/null {shlex.quote('http://' + service_ip + ':5001/v2/')}"
    guest(probe)
    apply("Gateway", "network", {"egress": {"allowlist": {"hosts": []}}})
    ax("resume", "task", "runner")
    wait_for("runner", "Running", "PoliciesApplied", seconds=60)
    empty_denied = ax("ssh", "runner", "--", "/bin/sh", "-c", probe, check=False).returncode != 0
    apply("Gateway", "network", {"egress": {"allowlist": {"hosts": [{"host": service_ip + "/32"}]}}})
    ax("resume", "task", "runner")
    wait_for("runner", "Running", "PoliciesApplied", seconds=60)
    cidr_allowed = ax("ssh", "runner", "--", "/bin/sh", "-c", probe, check=False).returncode == 0
    apply("Gateway", "network", {"egress": {"allowlist": {"hosts": [{"host": "192.0.2.0/24"}]}}})
    ax("resume", "task", "runner")
    wait_for("runner", "Running", "PoliciesApplied", seconds=60)
    cidr_denied = ax("ssh", "runner", "--", "/bin/sh", "-c", probe, check=False).returncode != 0
    report["egress_policy"] = {
        "destination": service_ip + ":5001/v2/", "allow_all_reachable": True,
        "empty_denied": empty_denied, "matching_cidr_allowed": cidr_allowed,
        "nonmatching_cidr_denied": cidr_denied,
    }
    if not (empty_denied and cidr_allowed and cidr_denied):
        raise RuntimeError("egress policy did not follow the expected allow/deny revisions")
    checks.append({"check": "egress-policy-revisions", "result": "passed"})
    print("PASS egress policy revisions", flush=True)

    ax("suspend", "task", "runner")
    wait_for("runner", "Suspended")
    apply("Task", "no-gateway", {"image": image, "debug": True,
                                  "command": ["/bin/sh", "-c", "echo ready > /workspace/no-gateway-ready"]})
    wait_for("no-gateway", "Running", "SetupComplete", seconds=150)
    ax("ssh", "no-gateway", "--", "/bin/sh", "-c", "test -f /workspace/no-gateway-ready")
    default_denied = ax("ssh", "no-gateway", "--", "/bin/sh", "-c", probe, check=False).returncode != 0
    report["egress_policy"]["no_gateway_denied"] = default_denied
    if not default_denied:
        raise RuntimeError("task without a Gateway reached the registry")
    checks.append({"check": "no-gateway-deny-default", "result": "passed"})
    print("PASS no-gateway default deny", flush=True)
finally:
    for name in ("no-gateway", "runner"):
        result = ax("suspend", "task", name, check=False)
        if result.returncode == 0:
            wait_for(name, "Suspended")
            report["final_" + name + "_phase"] = "Suspended"
    (output / "report.json").write_text(json.dumps(report, indent=2) + "\n")
print("Evidence: " + str(output / "report.json"), flush=True)
