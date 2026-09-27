# Editable verification policy

Project → Settings → Checks provides MVP, Balanced, Thorough and Custom presets. Categories are explicit; Blaxsmith does not infer a check's purpose from its command or invent missing tests.

| Category | MVP | Balanced | Thorough |
| --- | --- | --- | --- |
| Build / type check | Required | Required | Required |
| Focused tests / other | Advisory | Required | Required |
| Independent review command / E2E / security / performance | Off | Advisory | Required |

Every mode remains editable. Editing fields selects Custom. A review command is a configured executable; this selector does not create an independent model reviewer. Interview depth, model choice, human acceptance and concurrency remain separate controls.

`SetProjectVerification` takes `expected_version` (zero for initial creation), `preset`, and checks with explicit `category` and `mode`. The server rejects preset labels inconsistent with their modes. A stale version returns Connect `aborted`. The editor retains the unsaved draft; Cancel loads the current revision. Owners and admins can save; these permissions are not granted to machine tokens.

Each save appends an immutable policy revision. `ListProjectVerificationHistory` returns up to 50 revisions, newest first, with an exclusive `before_version` cursor. The settings history displays every configured command, mode and protected path for that revision. Historical rows predating revision tracking are not reconstructed; migration captures the current policy. Each run also retains its frozen policy and digest.

Changes apply to future admissions. They do not rewrite an active run, erase a failed result, or convert Off into a pass. Launch preview detects a policy change before admission and requires reconciliation. Tenant/session checks and immutable evidence are always enforced.

Qualification: `make e2e` exercises preset selection, mode edits, saved history, a concurrent writer, draft preservation, stale-save rejection and invalid preset labels against real HTTPS and PostgreSQL. Existing correction-loop E2E exercises required/advisory checks and retained failure evidence. Live AX/model qualification is separate.
