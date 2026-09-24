#!/usr/bin/env python3
"""Check a runner variant's pins against its Dockerfile and extension manifest.

usage: check-variant.py VARIANT   (run from anywhere; paths are repo-relative)
"""
import json
import pathlib
import re
import sys

root = pathlib.Path(__file__).resolve().parents[2]
variant = sys.argv[1] if len(sys.argv) == 2 else sys.exit(__doc__)
if not re.fullmatch(r'[a-z][a-z0-9-]{0,31}', variant):
    sys.exit('invalid variant name')
here = root / 'deploy/tool-worker'
pins = json.loads((here / f'{variant}-runtimes.json').read_text())
dockerfile = (here / f'{variant}.Dockerfile').read_text()
manifest = json.loads((root / pins['extension_manifest']).read_text())
runtimes = pins['runtimes']

if pins['variant'] != variant:
    sys.exit(f'{variant}-runtimes.json names variant {pins["variant"]}')
declared = {r['id']: r['version'] for r in manifest['runtimes']}
pinned = {k: v['version'] for k, v in runtimes.items()}
if declared != pinned:
    sys.exit(f'extension declares runtimes {declared}, variant pins {pinned}')
uv = runtimes['uv']
if not re.fullmatch(r'[0-9a-f]{64}', uv['sha256']) or f'/releases/download/{uv["version"]}/' not in uv['url']:
    sys.exit('uv pin needs a versioned release URL and a SHA-256')
required = [
    f'ADD --checksum=sha256:{uv["sha256"]} \\\n    {uv["url"]} ',
    f'uv python install {runtimes["python"]["version"]}',
    f'--python {runtimes["python"]["version"]} --exclude-newer {pins["exclude_newer"]} '
    f'{runtimes["serena"]["package"]}=={runtimes["serena"]["version"]}',
    'ENV UV_PYTHON_DOWNLOADS=never',
]
for text in required:
    if text not in dockerfile:
        sys.exit(f'{variant}.Dockerfile does not match its pins: missing {text.strip()!r}')
if re.search(r'\blatest\b', dockerfile):
    sys.exit(f'{variant}.Dockerfile must not float to latest')
print(f'{variant} variant pins match {pins["extension_manifest"]}: '
      + ', '.join(f'{k} {v}' for k, v in pinned.items()))
