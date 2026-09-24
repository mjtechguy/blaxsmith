-- Models a connection's managers pin as recommended; pickers list them first.
CREATE TABLE access_connection_recommended_models (
    organization_id text NOT NULL,
    connection_id text NOT NULL,
    model_id text NOT NULL CHECK (model_id ~ '^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$'),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 49),
    pinned_by text NOT NULL CHECK (length(pinned_by) BETWEEN 1 AND 160),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, connection_id, model_id),
    FOREIGN KEY (organization_id, connection_id) REFERENCES access_connections (organization_id, id)
);
