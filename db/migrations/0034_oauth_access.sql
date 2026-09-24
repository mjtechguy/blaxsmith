-- Personal coding-plan logins. The encrypted refresh material stays in
-- access_secret_versions; this row is the per-connection refresh lock and the
-- pointer to the newest refreshed version. Refresh never locks or updates
-- access_connections, so it cannot deadlock with release transactions that
-- hold FOR SHARE on the connection.
ALTER TABLE access_grants DROP CONSTRAINT access_grants_delivery_mode_check;
ALTER TABLE access_grants ADD CONSTRAINT access_grants_delivery_mode_check
    CHECK (delivery_mode IN ('brokered', 'short_lived', 'native_raw', 'oauth_access'));

CREATE TABLE access_oauth_sessions (
    organization_id text NOT NULL,
    connection_id text NOT NULL,
    secret_version bigint NOT NULL,
    access_expires_at timestamptz NOT NULL,
    refreshed_at timestamptz,
    reconnect_reason text CHECK (reconnect_reason IN ('refresh_rejected', 'refresh_outcome_unknown')),
    PRIMARY KEY (organization_id, connection_id),
    FOREIGN KEY (organization_id, connection_id)
        REFERENCES access_connections (organization_id, id),
    FOREIGN KEY (organization_id, connection_id, secret_version)
        REFERENCES access_secret_versions (organization_id, connection_id, version)
);
