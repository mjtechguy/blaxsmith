-- Anvil is the default factory. Other factories are imported explicitly.
CREATE OR REPLACE FUNCTION workflow_seed_recipes(org uuid) RETURNS void LANGUAGE plpgsql AS $fn$
DECLARE
    seeded uuid;
    body bytea := convert_to($recipe${
  "schema_version": "blaxsmith.recipe/v1alpha1",
  "name": "anvil-starter",
  "profiles": {
    "implementer": {
      "harness": "codex",
      "model": "gpt-5.6-luna",
      "effort": "xhigh"
    }
  },
  "stages": [
    {
      "id": "implement",
      "kind": "implement",
      "profile": "implementer",
      "prompt": "examples/anvil/implement.md"
    },
    {
      "id": "verify",
      "kind": "verify",
      "profile": "implementer",
      "prompt": "examples/anvil/verify.md",
      "depends_on": [
        "implement"
      ]
    }
  ],
  "required_checks": [],
  "limits": {
    "max_correction_cycles": 3,
    "timeout_seconds": 1800,
    "max_runtime_seconds": 0
  },
  "acceptance": "manual",
  "factory": {
    "id": "anvil",
    "version": "0.1.0"
  }
}
$recipe$, 'UTF8');
BEGIN
    INSERT INTO workflow_recipes (organization_id, name, description)
        VALUES (org, 'Anvil starter', 'Native implementation and user-selected checks. Manual acceptance by default; edit a version to choose policy acceptance.')
        ON CONFLICT DO NOTHING RETURNING id INTO seeded;
    IF seeded IS NOT NULL THEN
        INSERT INTO workflow_recipe_versions (organization_id, recipe_id, version, recipe_json, sha256, frozen_path)
            VALUES (org, seeded, 1, body, encode(sha256(body), 'hex'), 'examples/anvil/recipe.json');
        UPDATE workflow_recipes SET current_version_id = (SELECT id FROM workflow_recipe_versions
            WHERE organization_id = org AND recipe_id = seeded AND version = 1)
            WHERE organization_id = org AND id = seeded;
    END IF;
END $fn$;

-- Idempotent: only an organization's seeded recipe (created_by IS NULL) that
-- has never had a member-role grant gets one, so re-running the seed never
-- restores a grant an administrator revoked.
CREATE OR REPLACE FUNCTION workflow_seed_recipe_grants(org uuid) RETURNS void LANGUAGE sql AS $$
    INSERT INTO access_resource_grants (organization_id, resource_kind, resource_id, grantee_role, granted_by)
    SELECT r.organization_id, 'recipe', r.id::text, 'member', NULL
    FROM workflow_recipes r
    WHERE r.organization_id = org AND r.project_id IS NULL AND r.created_by IS NULL AND r.name = 'Anvil starter'
    AND NOT EXISTS (SELECT 1 FROM access_resource_grants g WHERE g.organization_id = r.organization_id
        AND g.resource_kind = 'recipe' AND g.resource_id = r.id::text AND g.grantee_role = 'member')
    ON CONFLICT DO NOTHING;
$$;

-- New organizations: the same trigger that seeds the recipe grants it.
CREATE OR REPLACE FUNCTION workflow_seed_recipes_trigger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM workflow_seed_recipes(NEW.id);
    PERFORM workflow_seed_recipe_grants(NEW.id);
    RETURN NEW;
END $$;

-- Seed Anvil for existing organizations without changing their recipe versions or grants.
SELECT workflow_seed_recipes(id) FROM identity_organizations;
SELECT workflow_seed_recipe_grants(id) FROM identity_organizations;
