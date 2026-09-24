-- The seeded Guild engineering organization recipe is usable by default:
-- a standing access_resource_grants row for the minimum role "member" (so
-- members, admins, and owners; never viewers). An installation seed has no
-- granting principal, so granted_by may now be NULL, as created_by is for
-- the seeded recipe itself.
ALTER TABLE access_resource_grants ALTER COLUMN granted_by DROP NOT NULL;

-- Idempotent: only an organization's seeded recipe (created_by IS NULL) that
-- has never had a member-role grant gets one, so re-running the seed never
-- restores a grant an administrator revoked.
CREATE FUNCTION workflow_seed_recipe_grants(org uuid) RETURNS void LANGUAGE sql AS $$
    INSERT INTO access_resource_grants (organization_id, resource_kind, resource_id, grantee_role, granted_by)
    SELECT r.organization_id, 'recipe', r.id::text, 'member', NULL
    FROM workflow_recipes r
    WHERE r.organization_id = org AND r.project_id IS NULL AND r.created_by IS NULL AND r.name = 'Guild engineering'
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

-- Existing organizations.
SELECT workflow_seed_recipe_grants(id) FROM identity_organizations;
