-- A released runner nonce is authority only when tied to the current owner
-- and the control-plane runtime observed during the release transaction.
ALTER TABLE bootstrap_challenges
    ADD COLUMN activation_nonce_sha256 bytea CHECK (activation_nonce_sha256 IS NULL OR octet_length(activation_nonce_sha256) = 32),
    ADD COLUMN activation_template_uid text,
    ADD COLUMN activation_image text,
    ADD COLUMN activation_worker_pool text;
