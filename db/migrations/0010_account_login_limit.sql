-- Cap attempts at one account across source addresses without checking whether
-- that account exists. This keeps the throttle independent of account lookup.
ALTER TABLE identity_login_limits
    DROP CONSTRAINT identity_login_limits_scope_check;

ALTER TABLE identity_login_limits
    ADD CONSTRAINT identity_login_limits_scope_check
    CHECK (scope IN ('source', 'account_source', 'account'));
