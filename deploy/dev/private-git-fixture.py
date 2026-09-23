#!/usr/bin/env python3
"""Synthetic, Basic-auth HTTPS Git fixture bound only to the k3s bridge."""

import base64
import datetime
import hmac
import http.server
import ipaddress
import os
import pathlib
import secrets
import ssl
import subprocess
import sys

from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

HOST = "10.42.0.1"
PORT = 8443


def prepare(root):
    root.mkdir(mode=0o700)
    www = root / "www"
    www.mkdir(mode=0o700)
    source = root / "source"
    source.mkdir(mode=0o700)

    def git(directory, *args):
        subprocess.run(("git", *args), cwd=directory, check=True, capture_output=True)

    git(source, "init", "-b", "main")
    git(source, "config", "user.name", "Blaxsmith Probe")
    git(source, "config", "user.email", "probe@example.invalid")
    (source / "README.md").write_text("blaxsmith-private-git-probe\n")
    git(source, "add", "README.md")
    git(source, "commit", "-m", "synthetic private fixture")
    git(root, "clone", "--bare", str(source), str(www / "private.git"))
    git(root, "--git-dir", str(www / "private.git"), "update-server-info")

    token_path = root / "token"
    fd = os.open(token_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        output.write(secrets.token_hex(24))

    key = ec.generate_private_key(ec.SECP256R1())
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "blaxsmith-private-git-probe")])
    now = datetime.datetime.now(datetime.timezone.utc)
    certificate = (
        x509.CertificateBuilder()
        .subject_name(name).issuer_name(name).public_key(key.public_key())
        .serial_number(x509.random_serial_number())
        .not_valid_before(now - datetime.timedelta(minutes=1))
        .not_valid_after(now + datetime.timedelta(days=1))
        .add_extension(x509.SubjectAlternativeName([x509.IPAddress(ipaddress.ip_address(HOST))]), critical=False)
        .add_extension(x509.BasicConstraints(ca=True, path_length=0), critical=True)
        .sign(key, hashes.SHA256())
    )
    (root / "ca.pem").write_bytes(certificate.public_bytes(serialization.Encoding.PEM))
    key_path = root / "server.key"
    fd = os.open(key_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(key.private_bytes(serialization.Encoding.PEM,
            serialization.PrivateFormat.TraditionalOpenSSL, serialization.NoEncryption()))


def serve(root):
    token = (root / "token").read_text()

    class Handler(http.server.SimpleHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def __init__(self, *args, **kwargs):
            super().__init__(*args, directory=str(root / "www"), **kwargs)

        def authorized(self):
            expected = "Basic " + base64.b64encode(("blaxsmith-probe:" + token).encode()).decode()
            if not hmac.compare_digest(self.headers.get("Authorization", ""), expected):
                self.send_response(401)
                self.send_header("WWW-Authenticate", 'Basic realm="blaxsmith-probe"')
                self.send_header("Content-Length", "0")
                self.end_headers()
                return False
            return True

        def do_GET(self):
            if self.authorized():
                super().do_GET()

        def do_HEAD(self):
            if self.authorized():
                super().do_HEAD()

    server = http.server.ThreadingHTTPServer((HOST, PORT), Handler)
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(root / "ca.pem", root / "server.key")
    server.socket = context.wrap_socket(server.socket, server_side=True)
    server.serve_forever()


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] not in ("prepare", "serve"):
        sys.exit("usage: private-git-fixture.py prepare|serve NEW_DIRECTORY")
    if sys.argv[1] == "prepare":
        prepare(pathlib.Path(sys.argv[2]))
    else:
        serve(pathlib.Path(sys.argv[2]))
