-- Members may use their own `claude setup-token` for runs they start
-- (docs/model-gateway-plan.md §6.1). Off by default; owners/admins opt in.
ALTER TABLE identity_organizations
  ADD COLUMN allow_member_claude_subscription boolean NOT NULL DEFAULT false;
