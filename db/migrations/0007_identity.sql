-- Local first-owner foundation. OIDC identities, MFA, sessions, and
-- resource-scoped permissions are added before this data is exposed by an API.
CREATE TABLE identity_principals (
    id uuid PRIMARY KEY,
    username text NOT NULL UNIQUE CHECK (username = lower(username) AND length(username) BETWEEN 3 AND 64),
    password_hash text CHECK (password_hash IS NULL OR password_hash <> ''),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE identity_organizations (
    id uuid PRIMARY KEY,
    slug text NOT NULL UNIQUE CHECK (slug = lower(slug) AND length(slug) BETWEEN 3 AND 64),
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 160),
    login_policy text NOT NULL DEFAULT 'local' CHECK (login_policy IN ('local', 'oidc', 'mixed')),
    mfa_policy text NOT NULL DEFAULT 'optional' CHECK (mfa_policy IN ('optional', 'required')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE identity_memberships (
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    principal_id uuid NOT NULL REFERENCES identity_principals (id),
    role text NOT NULL CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'disabled')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, principal_id)
);

CREATE TABLE identity_installation (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    first_organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    first_owner_id uuid NOT NULL REFERENCES identity_principals (id),
    bootstrapped_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE identity_audit_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    organization_id uuid NOT NULL REFERENCES identity_organizations (id),
    actor_kind text NOT NULL CHECK (actor_kind IN ('operator', 'principal', 'system')),
    actor_id uuid REFERENCES identity_principals (id),
    action text NOT NULL CHECK (action <> ''),
    subject_id uuid,
    occurred_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE INDEX identity_audit_by_organization ON identity_audit_events (organization_id, occurred_at DESC, id DESC);
