#!/usr/bin/env python3
"""Gate every dev AX Task with a controller-owned public key and pinned runner."""

import base64
import hashlib
import json
import pathlib
import stat
import subprocess
import sys

from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

if len(sys.argv) != 3:
    sys.exit("usage: enable-platform-bootstrap.py VERIFIED_AX_BUILD DEV_SIGNING_KEY_FILE")
build, key_file = pathlib.Path(sys.argv[1]).resolve(), pathlib.Path(sys.argv[2]).resolve()
record = json.loads((build / "provenance.json").read_text())
for component in ("ax-controller", "ax-task-runner"):
    if hashlib.sha256((build / component).read_bytes()).hexdigest() != record["binaries"][component]:
        sys.exit(f"{component} differs from verified build")
if key_file.stat().st_mode & (stat.S_IRWXG | stat.S_IRWXO):
    sys.exit("dev signing key must be owner-only")
key = Ed25519PrivateKey.from_private_bytes(key_file.read_bytes())
public = base64.b64encode(key.public_key().public_bytes(
    encoding=serialization.Encoding.Raw, format=serialization.PublicFormat.Raw)).decode()
images = [(build / f"{component}.image").read_text().strip()
          for component in ("ax-controller", "ax-task-runner")]
if any("@sha256:" not in image for image in images):
    sys.exit("AX images must be digest-pinned")

deployment = json.loads(subprocess.check_output([
    "kubectl", "-n", "ax-system", "get", "deployment", "ax-controller", "-o", "json",
]))
controller = next(c for c in deployment["spec"]["template"]["spec"]["containers"]
                  if c["name"] == "controller")
args = [arg for arg in controller["args"] if not arg.startswith((
    "--blaxsmith-bootstrap-public-key=", "--blaxsmith-runner-image=",
))]
args += ["--blaxsmith-bootstrap-public-key=" + public, "--blaxsmith-runner-image=" + images[1]]
annotations = {
    "blaxsmith.dev/ax-patch-sha256": record["patch_sha256"],
    "blaxsmith.dev/ax-egress-patch-sha256": record["egress_patch_sha256"],
    "blaxsmith.dev/ax-bootstrap-sha256": record["bootstrap_patch_sha256"],
    "blaxsmith.dev/ax-task-resources-patch-sha256": record["task_resources_patch_sha256"],
    "blaxsmith.dev/platform-key-patch-sha256": record["platform_key_patch_sha256"],
    "blaxsmith.dev/ax-build-provenance-sha256": hashlib.sha256((build / "provenance.json").read_bytes()).hexdigest(),
}
patch = {"spec": {"template": {
    "metadata": {"annotations": annotations},
    "spec": {"containers": [{"name": "controller", "image": images[0],
                            "command": ["/usr/local/bin/ax-controller"], "args": args}]},
}}}
patch_path = build / "controller-bootstrap-patch.json"
patch_path.write_text(json.dumps(patch, indent=2) + "\n")
subprocess.run(["kubectl", "-n", "ax-system", "patch", "deployment", "ax-controller",
                "--type=strategic", "--patch-file=" + str(patch_path)], check=True)
subprocess.run(["kubectl", "-n", "ax-system", "rollout", "status", "deployment/ax-controller",
                "--timeout=120s"], check=True)
