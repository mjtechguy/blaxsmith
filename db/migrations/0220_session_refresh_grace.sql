-- A rotated-out refresh token records the hash of the token that replaced it,
-- so a replay inside the short grace window (two tabs, or a response lost to a
-- server restart) can be answered with that same successor instead of being
-- treated as theft. The successor itself is derived, never stored.
ALTER TABLE identity_refresh_tokens
    ADD COLUMN successor_hash bytea CHECK (successor_hash IS NULL OR octet_length(successor_hash) = 32);
