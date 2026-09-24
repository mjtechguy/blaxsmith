-- Per-model metadata as the provider reports it at list time. The bundled
-- model manifest (internal/access/model_manifest.json) overlays legacy,
-- badge, default, and effort values at read time, so a release can update
-- them without refetching every connection.
ALTER TABLE access_connection_models
    ADD COLUMN is_default boolean NOT NULL DEFAULT false,
    ADD COLUMN legacy boolean NOT NULL DEFAULT false,
    ADD COLUMN badge text CHECK (badge IS NULL OR badge IN ('new')),
    ADD COLUMN efforts text[] NOT NULL DEFAULT '{}' CHECK (cardinality(efforts) <= 16),
    ADD COLUMN default_effort text CHECK (default_effort IS NULL OR default_effort ~ '^[a-z][a-z0-9_-]{0,31}$');
