# Pinned Guild code

`validate-spec.py` is copied without modification from
`plugins/forge/scripts/validate-spec.py` at Guild commit
`dda615434dfb4624e1ab6328851afc91ef58e5e1`. Its MIT license is included.

The Go wrapper embeds these reviewed bytes, runs them with Python's isolated mode,
and records the revision and SHA-256 in each bundle. It never runs a validator
selected by a candidate repository. Updates must compare upstream changes and
pass the Blaxsmith integration tests before changing this pin. Preserve legacy
v2.0 warnings and v2.1 enforcement; do not silently relax upstream checks.

The example stage prompts elsewhere in this repository are new Blaxsmith prompts,
not copies of Guild's full prompt packs. More Guild adoption requires its own
compatibility evidence; this copy does not imply Foundry dispatch is supported.
