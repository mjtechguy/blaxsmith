# Technology guide baseline and current deviations

Status: baseline adopted; deviations below remain release decisions.

The [Technology Selection Guide](https://technology-selection-guide.aws.ablabs.io/llms-full.txt)
snapshot inspected on 2026-09-22 and again on 2026-09-23 has SHA-256
`13ec22a5d0dfa4789b91e2e3101bed4d9e19982db444c8c66ad4173159e6ada0`.
Its version tables are dated; package and AX pins are verified separately.

Blaxsmith follows its Go/PostgreSQL/pgx control-plane direction, tenant keys
and foreign-key scoping, embedded transactional migrations with an advisory
lock, secure-cookie and resource-aware authorization requirements, Node 24,
and real-database CI. The user-selected Astronomer/TanStack frontend is the
explicit routing/form/table override. Connected v1 and deferred FIPS are
approved scope choices, not claims of guide conformance.

| Difference today | Reason and release gate |
|---|---|
| `db.Migrate` uses pgx directly instead of goose | The small runner embeds SQL, serializes startup, checks applied file hashes, and rejects newer unknown versions. It has no down/out-of-order migration support. Before P1 acceptance, either adopt goose with a tested history transition or explicitly approve this narrower runner and update the plan. |
| Secret versions use AES-256-GCM with organization/connection/version AAD instead of age | This has only synthetic probe evidence. Before real credentials, review the custody threat model and key recovery, then migrate to the guide's age baseline or document and approve the alternative. |
| The preview serves one public JSON catalog route rather than Connect-RPC | No account, project, or credential records are exposed. Product data must use generated Connect contracts and shared authorization before it reaches the browser. |
| CI currently runs Go, real PostgreSQL, and Node 24 builds | Add history-aware secret scanning, vulnerability/static checks, and production browser journeys before release. |

The first-owner password uses Argon2id at 64 MiB, three iterations, one lane,
with a random salt; this exceeds the [OWASP password-storage minimum](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html).
The operator command never accepts a password argument or environment variable.
The internal session service uses Ed25519 JWTs via `golang-jwt/jwt/v5`, ten-minute
access validity, seven-day server-side sessions, hashed single-use refresh
tokens, current membership/policy checks, and auditable revocation/replay.
Its signing key is in memory only in tests; persistent key loading/rotation,
login throttling, and browser transport are not implemented. No login/session
endpoint is exposed yet. Browser sessions will require
`HttpOnly`, `Secure`, `SameSite` cookies plus CSRF/origin protection as described
by the [OWASP session](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)
and [CSRF](https://cheatsheetseries.owasp.org/cheatsheets/Cross-Site_Request_Forgery_Prevention_Cheat_Sheet.html)
guides.
