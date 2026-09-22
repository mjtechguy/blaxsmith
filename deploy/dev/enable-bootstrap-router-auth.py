#!/usr/bin/env python3
"""Deploy the verified dev router with connector-only bootstrap TokenReview."""

import hashlib
import json
import pathlib
import subprocess
import sys

if len(sys.argv) != 2:
    sys.exit("usage: enable-bootstrap-router-auth.py VERIFIED_BUILD_DIRECTORY")
build = pathlib.Path(sys.argv[1]).resolve()
record = json.loads((build / "provenance.json").read_text())
if hashlib.sha256((build / "atenet").read_bytes()).hexdigest() != record["binary_sha256"]:
    sys.exit("router binary differs from verified build")
image = (build / "atenet.image").read_text().strip()
if "@sha256:" not in image:
    sys.exit("published router image is not digest-pinned")

manifest = pathlib.Path(__file__).with_name("bootstrap-router-auth.yaml")
subprocess.run(["kubectl", "apply", "-f", str(manifest)], check=True)
deployment = json.loads(subprocess.check_output([
    "kubectl", "-n", "ate-system", "get", "deployment", "atenet-router", "-o", "json",
]))
router = next(container for container in deployment["spec"]["template"]["spec"]["containers"]
              if container["name"] == "atenet-router")
args = [arg for arg in router["args"] if not arg.startswith((
    "--bootstrap-audience=", "--bootstrap-client-username=",
))]
args += ["--bootstrap-audience=blaxsmith-bootstrap",
         "--bootstrap-client-username=system:serviceaccount:ate-system:blaxsmith-connector"]
patch = {"spec": {"template": {
    "metadata": {"annotations": {"blaxsmith.dev/router-auth-patch-sha256": record["router_auth_patch_sha256"],
                             "blaxsmith.dev/actor-fence-patch-sha256": record["actor_fence_patch_sha256"]}},
    "spec": {"containers": [{"name": "atenet-router", "image": image,
                            "command": ["/usr/local/bin/atenet"], "args": args}]},
}}}
patch_path = build / "router-auth-deployment-patch.json"
patch_path.write_text(json.dumps(patch, indent=2) + "\n")
subprocess.run(["kubectl", "-n", "ate-system", "patch", "deployment", "atenet-router",
                "--type=strategic", "--patch-file=" + str(patch_path)], check=True)
subprocess.run(["kubectl", "-n", "ate-system", "rollout", "status", "deployment/atenet-router",
                "--timeout=120s"], check=True)
