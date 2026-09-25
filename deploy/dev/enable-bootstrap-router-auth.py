#!/usr/bin/env python3
"""Deploy the verified dev router with connector-only bootstrap TokenReview
(and guest gRPC auth when the build carries guest-router-auth.patch)."""

import difflib
import hashlib
import json
import os
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

# ROUTER_AUTH_DIFF=1 prints the Deployment change (server dry-run) and exits.
diff_only = os.environ.get("ROUTER_AUTH_DIFF") == "1"
dry_run = ["--dry-run=server"] if diff_only else []
manifest = pathlib.Path(__file__).with_name("bootstrap-router-auth.yaml")
# In diff mode stdout carries only the Deployment diff (empty: nothing to do).
subprocess.run(["kubectl", "apply", "-f", str(manifest), *dry_run], check=True,
               stdout=sys.stderr if diff_only else None)
deployment = json.loads(subprocess.check_output([
    "kubectl", "-n", "ate-system", "get", "deployment", "atenet-router", "-o", "json",
]))
router = next(container for container in deployment["spec"]["template"]["spec"]["containers"]
              if container["name"] == "atenet-router")
# Guest gRPC auth (guest-router-auth.patch) is never silently dropped: the app
# sends the connector token on guest calls, and an unpatched router would
# forward it into the guest.
guest_patch = record.get("guest_router_auth_patch_sha256")
if "--guest-client-auth" in router["args"] and not guest_patch:
    sys.exit("refusing to deploy a router build without guest-router-auth.patch over --guest-client-auth")
args = [arg for arg in router["args"] if not arg.startswith((
    "--bootstrap-audience=", "--bootstrap-client-username=", "--guest-client-auth", "--guest-client-username=",
))]
args += ["--bootstrap-audience=blaxsmith-bootstrap",
         "--bootstrap-client-username=system:serviceaccount:ate-system:blaxsmith-connector"]
annotations = {"blaxsmith.dev/router-auth-patch-sha256": record["router_auth_patch_sha256"],
               "blaxsmith.dev/actor-fence-patch-sha256": record["actor_fence_patch_sha256"]}
# GUEST_CLIENT_USERNAME: the app's own ServiceAccount (projected, kubelet-rotated
# token); unset, guest routes accept the bootstrap connector identity.
guest_username = os.environ.get("GUEST_CLIENT_USERNAME", "")
if guest_username and not guest_patch:
    sys.exit("GUEST_CLIENT_USERNAME needs a build with guest-router-auth.patch")
if guest_patch:
    args.append("--guest-client-auth")
    if guest_username:
        args.append("--guest-client-username=" + guest_username)
    annotations["blaxsmith.dev/guest-router-auth-patch-sha256"] = guest_patch
patch = {"spec": {"template": {
    "metadata": {"annotations": annotations},
    "spec": {"containers": [{"name": "atenet-router", "image": image,
                            "command": ["/usr/local/bin/atenet"], "args": args}]},
}}}
patch_path = build / "router-auth-deployment-patch.json"
patch_path.write_text(json.dumps(patch, indent=2) + "\n")
command = ["kubectl", "-n", "ate-system", "patch", "deployment", "atenet-router",
           "--type=strategic", "--patch-file=" + str(patch_path)]
if diff_only:
    after = json.loads(subprocess.check_output(command + dry_run + ["-o", "json"]))
    show = lambda d: json.dumps(d["spec"]["template"], indent=2, sort_keys=True).splitlines()
    sys.stdout.writelines(line + "\n" for line in difflib.unified_diff(
        show(deployment), show(after), "atenet-router (live)", "atenet-router (patched)", lineterm=""))
    sys.exit(0)
subprocess.run(command, check=True)
subprocess.run(["kubectl", "-n", "ate-system", "rollout", "status", "deployment/atenet-router",
                "--timeout=120s"], check=True)
