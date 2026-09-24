# Project file: `.blaxsmith.json`

A repository can tell Blaxsmith how to verify and prepare it by committing
`.blaxsmith.json` at its root. The schema is
[`project-file.schema.json`](project-file.schema.json) (version 1).

```json
{
  "$schema": "https://github.com/mjtechguy/blaxsmith/docs/project-file.schema.json",
  "version": 1,
  "verification": [
    { "id": "unit-tests", "command": ["go", "test", "./..."] },
    { "id": "lint", "command": ["make", "lint"] }
  ],
  "setup": [
    { "id": "deps", "command": ["go", "mod", "download"] }
  ],
  "recipe": "Guild engineering"
}
```

| Field | Meaning |
|---|---|
| `version` | Required. Always `1`; other versions are ignored with an error. |
| `verification` | Up to 64 checks. Each `id` is lowercase (`^[a-z][a-z0-9_-]{0,63}$`) and unique; each `command` is 1–32 non-empty arguments run exactly, with no shell. |
| `setup` | Up to 16 commands that prepare a workspace, such as installing dependencies. Same rules as checks. |
| `recipe` | The name of a library recipe (organization or project) to preselect for new runs. |

Unknown fields, trailing data, or any invalid value make the whole file
invalid; Blaxsmith then says why and falls back to detection.

## How it is used

`SetupService.InspectRepository` fetches the project's Git source at its
pinned commit through the same path runs use (the project's Git connection
for a private repository) and reads the file there. Nothing is saved
automatically:

- **Verification settings** prefill the checks when none are configured, or
  offer "Use suggested checks"; an owner or admin reviews and saves them.
- **Source settings** show what was found after the repository is chosen, with
  a link to review the suggested checks.
- **New run** preselects the suggested recipe when one with that name is
  available to the project.

Setup commands are shown for reference; Blaxsmith does not run them yet.

## Detection without the file

Without a valid file, root manifests suggest commands:

| Found | Checks | Setup |
|---|---|---|
| `package.json` scripts `test`, `lint`, `typecheck` | `<pm> run <script>` (`npm test` for npm) | `pnpm install --frozen-lockfile`, `yarn install --frozen-lockfile` (`--immutable` with `.yarnrc.yml`), `bun install --frozen-lockfile`, `npm ci` (with `package-lock.json`) or `npm install` |
| `go.mod` | `go test ./...`, `go vet ./...` | `go mod download` |
| `Makefile` targets `test`, `lint`, `check` | `make <target>` | — |
| pytest configuration (`pyproject.toml`, `pytest.ini`, `conftest.py`, `setup.cfg`, `tox.ini`); `[tool.ruff]` | `python -m pytest`, `ruff check .` (through `uv run` or `poetry run` when locked) | `uv sync` or `poetry install` |
| `Cargo.toml` | `cargo test` | `cargo fetch` |

The package manager comes from the lockfile (`pnpm-lock.yaml`, `yarn.lock`,
`bun.lock`/`bun.lockb`, `package-lock.json`), else the `packageManager`
field, else npm. npm's placeholder `test` script is ignored.
