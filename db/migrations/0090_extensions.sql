-- Extension packs (docs/extensions-and-runtimes.md): an organization installs
-- a Git repository at one commit with a validated manifest and an approved
-- permission set. Versions are immutable; runs record the exact versions they
-- froze. The platform stores bytes only and never runs extension content.
CREATE TABLE workflow_extensions (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    extension_key text NOT NULL CHECK (extension_key ~ '^[a-z][a-z0-9-]{0,63}$'),
    repository_url text NOT NULL CHECK (length(repository_url) BETWEEN 1 AND 2048),
    -- The ref the administrator installs from; "update available" compares
    -- its latest resolution with the current version's commit.
    git_ref text NOT NULL CHECK (length(git_ref) BETWEEN 1 AND 128),
    current_version_id uuid,
    latest_ref_commit text CHECK (latest_ref_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    latest_checked_at timestamptz,
    created_by uuid NOT NULL REFERENCES identity_principals (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, extension_key)
);

CREATE TABLE workflow_extension_versions (
    organization_id uuid NOT NULL,
    id uuid NOT NULL DEFAULT gen_random_uuid(),
    extension_id uuid NOT NULL,
    version text NOT NULL CHECK (length(version) BETWEEN 5 AND 96),
    repository_url text NOT NULL CHECK (length(repository_url) BETWEEN 1 AND 2048),
    git_ref text NOT NULL CHECK (length(git_ref) BETWEEN 1 AND 128),
    commit_sha text NOT NULL CHECK (commit_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$'),
    -- Exact validated bytes; jsonb would normalize them and change the digest.
    manifest_json bytea NOT NULL CHECK (octet_length(manifest_json) BETWEEN 1 AND 65536),
    manifest_sha256 text NOT NULL,
    manifest_origin text NOT NULL CHECK (manifest_origin IN ('repository', 'overlay')),
    manifest_path text NOT NULL DEFAULT '' CHECK (length(manifest_path) <= 512),
    -- Sorted permission ids the administrator approved at install.
    approved_permissions text[] NOT NULL,
    permissions_sha256 text NOT NULL CHECK (permissions_sha256 ~ '^[0-9a-f]{64}$'),
    installed_by uuid NOT NULL REFERENCES identity_principals (id),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    UNIQUE (organization_id, extension_id, id),
    UNIQUE (organization_id, extension_id, version),
    FOREIGN KEY (organization_id, extension_id) REFERENCES workflow_extensions (organization_id, id),
    CHECK (manifest_sha256 = encode(sha256(manifest_json), 'hex')),
    CHECK ((manifest_origin = 'repository') = (manifest_path <> ''))
);

ALTER TABLE workflow_extensions ADD FOREIGN KEY (organization_id, id, current_version_id)
    REFERENCES workflow_extension_versions (organization_id, extension_id, id);

CREATE FUNCTION workflow_extension_versions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'extension versions are immutable' USING ERRCODE = 'check_violation';
END $$;
CREATE TRIGGER workflow_extension_versions_immutable BEFORE UPDATE OR DELETE ON workflow_extension_versions
    FOR EACH ROW EXECUTE FUNCTION workflow_extension_versions_immutable();

-- Provenance: the extension versions a run froze, with the digests the bundle
-- recorded. The bundle digest covers the same values.
CREATE TABLE workflow_run_extensions (
    organization_id uuid NOT NULL,
    run_id uuid NOT NULL,
    extension_version_id uuid NOT NULL,
    manifest_sha256 text NOT NULL,
    permissions_sha256 text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, run_id, extension_version_id),
    FOREIGN KEY (organization_id, run_id) REFERENCES workflow_runs (organization_id, id),
    FOREIGN KEY (organization_id, extension_version_id) REFERENCES workflow_extension_versions (organization_id, id)
);

-- Extensions are organization resources granted like recipes (access.CanUse).
ALTER TABLE access_resource_grants DROP CONSTRAINT access_resource_grants_resource_kind_check;
ALTER TABLE access_resource_grants ADD CONSTRAINT access_resource_grants_resource_kind_check
    CHECK (resource_kind IN ('connection', 'recipe', 'extension'));
