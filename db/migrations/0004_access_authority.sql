-- Non-secret authority records. Secret custody and provider issuance are separate.
-- Every reference includes organization_id; an ID from another organization
-- cannot satisfy a foreign key or a policy lookup.
CREATE TABLE access_provider_registrations (
    organization_id text NOT NULL CHECK (organization_id <> ''),
    id text NOT NULL CHECK (id <> ''),
    provider_kind text NOT NULL CHECK (provider_kind <> ''),
    origin text NOT NULL CHECK (origin <> ''),
    delivery_modes text[] NOT NULL CHECK (cardinality(delivery_modes) > 0),
    state text NOT NULL CHECK (state IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id)
);

CREATE TABLE access_connections (
    organization_id text NOT NULL CHECK (organization_id <> ''),
    id text NOT NULL CHECK (id <> ''),
    owner_kind text NOT NULL CHECK (owner_kind IN ('user', 'organization')),
    owner_id text NOT NULL CHECK (owner_id <> ''),
    provider_registration_id text NOT NULL CHECK (provider_registration_id <> ''),
    external_account_id text NOT NULL CHECK (external_account_id <> ''),
    auth_method text NOT NULL CHECK (auth_method <> ''),
    state text NOT NULL CHECK (state IN ('active', 'disabled', 'revoked')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, provider_registration_id)
        REFERENCES access_provider_registrations (organization_id, id)
);

CREATE TABLE access_project_policies (
    organization_id text NOT NULL CHECK (organization_id <> ''),
    project_id text NOT NULL CHECK (project_id <> ''),
    version bigint NOT NULL CHECK (version > 0),
    git_read_enabled boolean NOT NULL,
    delivery_modes text[] NOT NULL CHECK (cardinality(delivery_modes) > 0),
    PRIMARY KEY (organization_id, project_id)
);

CREATE TABLE access_grants (
    organization_id text NOT NULL CHECK (organization_id <> ''),
    id text NOT NULL CHECK (id <> ''),
    connection_id text NOT NULL,
    project_id text NOT NULL CHECK (project_id <> ''),
    grantee_kind text NOT NULL CHECK (grantee_kind IN ('user', 'team', 'workload')),
    grantee_id text NOT NULL CHECK (grantee_id <> ''),
    capability text NOT NULL CHECK (capability <> ''),
    resource text NOT NULL CHECK (resource <> ''),
    delivery_mode text NOT NULL CHECK (delivery_mode IN ('brokered', 'short_lived', 'native_raw')),
    issuer_id text NOT NULL CHECK (issuer_id <> ''),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    expires_at timestamptz,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, connection_id)
        REFERENCES access_connections (organization_id, id),
    FOREIGN KEY (organization_id, project_id)
        REFERENCES access_project_policies (organization_id, project_id)
);

CREATE TABLE access_bindings (
    organization_id text NOT NULL CHECK (organization_id <> ''),
    id text NOT NULL CHECK (id <> ''),
    attempt_id text NOT NULL CHECK (attempt_id <> ''),
    project_id text NOT NULL CHECK (project_id <> ''),
    grant_id text NOT NULL,
    grant_version bigint NOT NULL CHECK (grant_version > 0),
    capability text NOT NULL CHECK (capability <> ''),
    resource text NOT NULL CHECK (resource <> ''),
    input_commit text,
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, id),
    FOREIGN KEY (organization_id, grant_id)
        REFERENCES access_grants (organization_id, id),
    FOREIGN KEY (organization_id, project_id)
        REFERENCES access_project_policies (organization_id, project_id),
    CHECK (input_commit IS NULL OR input_commit ~ '^[0-9a-f]{40}([0-9a-f]{24})?$')
);

CREATE INDEX access_bindings_attempt ON access_bindings (organization_id, attempt_id);
