-- Use of an organization-level resource (a connection or a recipe) by a
-- project, a member, or every member at or above an organization role.
-- access_grants stays the per-project credential delegation record (capability,
-- resource scope, delivery mode, bindings, leases); this table only answers
-- "may this principal/project use that org resource at all".
CREATE TABLE access_resource_grants (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    resource_kind text NOT NULL CHECK (resource_kind IN ('connection', 'recipe')),
    resource_id text NOT NULL CHECK (resource_id <> '' AND length(resource_id) <= 200),
    grantee_project_id uuid,
    grantee_principal_id uuid,
    grantee_role text CHECK (grantee_role IN ('owner', 'admin', 'member')),
    granted_by uuid NOT NULL REFERENCES identity_principals (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, grantee_project_id)
        REFERENCES workflow_projects (organization_id, id),
    FOREIGN KEY (organization_id, grantee_principal_id)
        REFERENCES identity_memberships (organization_id, principal_id),
    CHECK (num_nonnulls(grantee_project_id, grantee_principal_id, grantee_role) = 1)
);

CREATE UNIQUE INDEX access_resource_grants_live ON access_resource_grants (organization_id, resource_kind, resource_id,
    (COALESCE(grantee_project_id::text, grantee_principal_id::text, grantee_role))) WHERE revoked_at IS NULL;
