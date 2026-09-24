-- Managed recipe library: organization- or project-scoped recipes with
-- immutable, validated versions. Prompt and skill files referenced by a version
-- stay repository paths that the launch freeze resolves against the project's
-- commit, exactly as a committed recipe file is frozen.
CREATE TABLE workflow_recipes (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    project_id uuid, -- NULL: organization scope.
    name text NOT NULL CHECK (name = btrim(name) AND length(name) BETWEEN 1 AND 120),
    description text NOT NULL DEFAULT '' CHECK (length(description) <= 2000),
    current_version_id uuid,
    created_by uuid REFERENCES identity_principals (id), -- NULL: installation seed.
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES workflow_projects (organization_id, id)
);
CREATE UNIQUE INDEX workflow_recipes_scope_name ON workflow_recipes
    (organization_id, COALESCE(project_id, '00000000-0000-0000-0000-000000000000'::uuid), lower(name));
CREATE INDEX workflow_recipes_by_project ON workflow_recipes (organization_id, project_id);

CREATE TABLE workflow_recipe_versions (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    recipe_id uuid NOT NULL,
    version integer NOT NULL CHECK (version >= 1),
    -- Exact validated bytes; jsonb would normalize them and change the digest.
    recipe_json bytea NOT NULL CHECK (octet_length(recipe_json) BETWEEN 1 AND 1048576),
    sha256 text NOT NULL,
    -- Repository-relative path label the bundle records for this recipe.
    frozen_path text NOT NULL CHECK (length(frozen_path) BETWEEN 1 AND 512),
    author_principal_id uuid REFERENCES identity_principals (id), -- NULL: installation seed.
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, recipe_id, id),
    UNIQUE (organization_id, recipe_id, version),
    FOREIGN KEY (organization_id, recipe_id) REFERENCES workflow_recipes (organization_id, id),
    CHECK (sha256 = encode(sha256(recipe_json), 'hex'))
);

ALTER TABLE workflow_recipes ADD FOREIGN KEY (organization_id, id, current_version_id)
    REFERENCES workflow_recipe_versions (organization_id, recipe_id, id);

CREATE FUNCTION workflow_recipe_versions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'recipe versions are immutable' USING ERRCODE = 'check_violation';
END $$;
CREATE TRIGGER workflow_recipe_versions_immutable BEFORE UPDATE OR DELETE ON workflow_recipe_versions
    FOR EACH ROW EXECUTE FUNCTION workflow_recipe_versions_immutable();

-- Library provenance for runs launched from a version; the bundle digest
-- still records the exact frozen bytes either way.
ALTER TABLE workflow_runs ADD COLUMN recipe_version_id uuid;
ALTER TABLE workflow_runs ADD FOREIGN KEY (organization_id, recipe_version_id)
    REFERENCES workflow_recipe_versions (organization_id, id);

-- Seed: every organization, existing or created later, gets the Guild
-- engineering recipe (examples/guild/recipe.json) as version 1. This runs in
-- the database so an organization created by bootstrap after startup is seeded
-- too, and the name conflict makes it idempotent.
CREATE FUNCTION workflow_seed_recipes(org uuid) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
    seeded uuid;
    body bytea := convert_to($recipe${
  "schema_version": "blaxsmith.recipe/v1alpha1",
  "name": "guild-engineering",
  "profiles": {
    "architect": {
      "harness": "claude-code",
      "model": "opus",
      "effort": "high",
      "skills": ["examples/guild/skills/evidence/SKILL.md"]
    },
    "implementer": {
      "harness": "codex",
      "model": "gpt-5.6-luna",
      "effort": "xhigh"
    },
    "reviewer": {
      "harness": "claude-code",
      "model": "opus",
      "effort": "high",
      "skills": ["examples/guild/skills/evidence/SKILL.md"]
    }
  },
  "stages": [
    {"id": "plan", "kind": "plan", "profile": "architect", "prompt": "examples/guild/prompts/plan.md"},
    {"id": "implement", "kind": "implement", "profile": "implementer", "depends_on": ["plan"], "prompt": "examples/guild/prompts/implement.md"},
    {"id": "review", "kind": "review", "profile": "reviewer", "depends_on": ["implement"], "prompt": "examples/guild/prompts/review.md"},
    {"id": "verify", "kind": "verify", "profile": "reviewer", "depends_on": ["implement"], "prompt": "examples/guild/prompts/verify.md", "loop": {"with": "implement", "until": "pass", "max_cycles": 3}},
    {"id": "architect-review", "kind": "architect_review", "profile": "architect", "depends_on": ["review", "verify"], "prompt": "examples/guild/prompts/architect-review.md"},
    {"id": "human-review", "kind": "human_review", "depends_on": ["architect-review"]}
  ],
  "required_checks": ["project-tests", "requirement-coverage"],
  "limits": {"max_correction_cycles": 3, "timeout_seconds": 1800, "max_runtime_seconds": 0}
}
$recipe$, 'UTF8');
BEGIN
    INSERT INTO workflow_recipes (organization_id, name, description)
        VALUES (org, 'Guild engineering', 'Plan, implement, review, verify with a bounded correction loop, architect review, then human review.')
        ON CONFLICT DO NOTHING RETURNING id INTO seeded;
    IF seeded IS NULL THEN
        RETURN;
    END IF;
    INSERT INTO workflow_recipe_versions (organization_id, recipe_id, version, recipe_json, sha256, frozen_path)
        VALUES (org, seeded, 1, body, encode(sha256(body), 'hex'), 'examples/guild/recipe.json');
    UPDATE workflow_recipes SET current_version_id = (SELECT id FROM workflow_recipe_versions
        WHERE organization_id = org AND recipe_id = seeded AND version = 1)
        WHERE organization_id = org AND id = seeded;
END $fn$;

CREATE FUNCTION workflow_seed_recipes_trigger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM workflow_seed_recipes(NEW.id);
    RETURN NEW;
END $$;
CREATE TRIGGER workflow_seed_recipes AFTER INSERT ON identity_organizations
    FOR EACH ROW EXECUTE FUNCTION workflow_seed_recipes_trigger();

SELECT workflow_seed_recipes(id) FROM identity_organizations;
