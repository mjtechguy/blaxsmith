# Git-backed Guild recipes: first implementation

Git stores authorable inputs: recipe definitions, explicit tool/model/effort
profiles, prompts, skills, repository instructions, specifications, and interview
transcripts. A recipe does not contain a particular ticket's specification:
`check`/`freeze` bind those separate inputs so the same team recipe can be reused.

The platform will own tenants, RBAC, approved versions, connections, grants,
bindings, leases, live workflow state, conversations, check policy, and final
human decisions. A Git commit is provenance, not authorization. A compiled
bundle is not a run or an approval, and its digest is not a signature.

## Working commands

Requires Go 1.27.1, Git, and Python 3.10+. No Go dependencies are added.

```sh
make check
make build
make example

./bin/blaxsmith freeze \
  --repo . --ref HEAD \
  --recipe examples/guild/recipe.json \
  --spec examples/guild/spec.md \
  --transcript examples/guild/transcript.md \
  --scope examples/guild > /tmp/blaxsmith-bundle.json
```

Only committed files at the selected ref are read. Commit your recipe changes
before checking them. The compiler resolves the ref once and then reads Git
objects directly; dirty files, branch movement after resolution, Git replacement
objects, and checkout filters cannot change those selected bytes. It does not
fetch missing objects, execute recipe commands, or start any agent tool.

The example's transcript is synthetic test data, not a record of user decisions.
Model names and effort values are explicit example settings, not claims about
availability in an account. Adapter preflight must eventually validate the exact
combination and return a blocker when unsupported; no automatic substitution.

## Recipe contract

`blaxsmith.recipe/v1alpha1` is a deliberately provisional JSON format, implemented
in `internal/recipe`. Unknown fields and duplicate JSON keys fail validation.
Paths are relative to the repository root. Profiles select a harness, model,
effort, and explicit instruction/skill files. A skill with supporting files must
list each file; the compiler does not infer Markdown imports or execute hooks.

Supported stage kinds are `plan`, `implement`, `review`, `verify`,
`architect_review`, `human_review`, `research`, `integrate`, `ui_review`, and
`documentation`. Dependencies form an acyclic graph. Planning must precede each
implementation; review and verification must follow it. All agent work must
precede the single final architect review, which precedes the single human
review. Human review has no agent profile or prompt. Multiple implement/review
stages and parallel review/verification are supported by the graph.

Profiles currently accept `claude-code` and `codex`, matching launch scope.
This compiler is harness-independent; actual launch adapters are not implemented.
OpenCode and Grok require adapter capability evidence before being admitted.

Limits are explicit: 1–10 correction cycles and 1–86400 timeout seconds. These
are initial compiler bounds, not measured workflow defaults. The graph captures
the forward path; correction dispatch, retries, per-stage usage budgets,
escalations, and adjudication are still controller work. The compiler validates
limits but cannot enforce elapsed execution time because it starts no execution.

`required_checks` contains names, never executable shell commands. Future launch
admission must merge them with mandatory organization/project policy and freeze
the resolved trusted check definitions separately. The recipe cannot establish
the authority to modify tests, waive failures, or publish a candidate.

## Frozen bundle

`blaxsmith.bundle/v1alpha1` records the full source commit, recipe/spec/transcript
paths, source scope, parsed recipe, deterministic stage order, and sorted
artifacts. Every artifact has its original bytes encoded as JSON base64 and a
SHA-256 digest. Included artifacts are:

- Recipe, specification, and transcript.
- Every declared stage prompt and profile instruction/skill file.
- Root and ancestor `AGENTS.md` files plus nested `AGENTS.md` within the selected
  scope. Paths remain intact so nested instructions retain their directory scope;
  an adapter must not apply every nested instruction globally.

The pinned Guild Forge validator runs over the frozen specification and
transcript. Its exact source revision, content hash, and output accompany the
bundle. Failure prevents bundle output. Upstream v2.0 compatibility warnings are
retained; the example exercises v2.1's stricter typed-table checks. This gate
checks specification fidelity and coverage, not implementation correctness.

The bundle digest is SHA-256 over Go `encoding/json.Marshal` of the bundle with
the `digest` field omitted. Artifact order is lexical; dependency ordering uses
declaration order for ties; timestamps and local filesystem paths are absent.
For cross-language consumption, define a standard canonical encoding before
making this alpha digest a public API or signing format. Keep source commit and
per-file hashes regardless of later serialization changes.

Compiler limits: 64 stages, 256 artifacts, 1 MiB per artifact, 16 MiB of artifact
content. Selected artifacts must be regular UTF-8 text without NUL bytes.
Symlink artifacts, unresolved LFS pointers, path traversal, and submodules in
scope are rejected. Code files themselves are referenced through the source
commit, not copied into the instruction bundle. Git refs must be retained by
future artifact retention; a digest alone does not keep a repository available.

## What follows

The first slice binds all inputs from one repository and one commit. Add managed
recipe repositories and separate project commits when the platform importer and
version policy exist; retain an independent revision and digest for each source.
UI editing should export the same versioned representation and show its diff.
An edit creates a new version and cannot retune an active run.

Before dispatch, Blaxsmith still needs authenticated admission, approved recipe
and profile versions, frozen check policy, Connection → Grant → Binding → Lease,
capability validation, and AX assignment/result contracts. Repository text cannot
grant those capabilities. Do not put credentials into recipes or prompt files:
the current compiler preserves selected bytes and is not a secret scanner.

Recipe experiments will use the same frozen project/spec/check inputs, with
separately versioned profile or stage changes. Quality comparisons, isolated
execution, and the final human gate belong to the platform, not Git hooks.
