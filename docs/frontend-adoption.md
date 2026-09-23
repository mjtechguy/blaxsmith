# Astronomer frontend adoption inventory

The local reference is `../reference/astronomer` at commit
`5961992098c8d8c72a5159e966359ad61838579d`. Its top-level `LICENSE`
and README identify **AGPL-3.0-or-later**. No Astronomer frontend source,
styles, assets, or screenshots have been copied into Blaxsmith. Direct source
porting needs an explicit product licensing decision; this inventory records
the chosen design and behavior without making that decision implicitly.

The pinned reference frontend uses Node `>=24.21.0 <25`, React `19.3.0`,
TypeScript `6.0.3`, Vite `8.3.0`, Tailwind `4.3.3`, TanStack Router `1.170.35`,
Query `5.102.8`, Table `9.2.4`, Form `1.33.5`, Pacer `0.23.0`, and Virtual
`3.14.11` in its lockfile. Blaxsmith's current local Node 25.8.0 is outside
that reference engine range, so frontend validation should use the pinned
Node 24 toolchain. These are reference versions, not a claim that they are
the latest upstream releases.

| Reference source | Blaxsmith destination and required adaptation |
|---|---|
| `frontend/src/styles/globals.css`, `lib/theme.tsx` | Shared Tailwind 4 theme, typography, semantic state colors, light/dark/system preference, first-paint theme bootstrap, reduced motion, and focus styles. Replace reference branding and storage keys. |
| `components/layout/sidebar.tsx`, `sidebar-navigation.ts`, `sidebar-navigation-view.tsx`, `topbar.tsx` | Persistent project/organization sidebar and topbar. Preserve the 240 px expanded / 64 px collapsed sidebar and 56 px header rhythm, responsive navigation, breadcrumbs, theme and account controls. Resolve visible items from product RBAC; do not carry cluster-specific API hooks. |
| `routes/dashboard/route.tsx`, `lib/dashboard-content-layout.ts`, `components/ui/page.tsx` | Shared authenticated shell, route error and not-found states, 1800 px contained pages and full-width work surfaces. Replace cluster routes with inbox, projects, runs, recipes, connections, tools, and administration. |
| `components/ui/action-button.tsx`, `card.tsx`, `badge.tsx`, `status-badge.tsx`, `empty-state.tsx`, `overlay-shell.tsx` | One shared control/status/empty-state vocabulary. Use confirmation modals only; creation and configuration get routed pages. No drawers, browser dialogs, or popup windows. |
| `components/form/fields.tsx`, `error-summary.tsx`, `secrets.ts`, `lib/form.ts` | TanStack Form field kit for grouped pages, accessible errors, retained invalid input, and secret-safe editing. No bare forms. |
| `components/ui/data-table*.tsx` | Canonical TanStack Table for runs, projects, connections, and tools, including scoped server paging/search/sort, stable IDs, density, visibility, query states, and virtual rows where useful. Do not replace it with per-page grids. |
| `routes/auth/login/index.tsx` | Split-panel responsive login styling, adapted to local username/password, optional OIDC, TOTP, first-owner setup, and reset. Do not copy the reference's email-only assumptions or wire a fake login before product auth exists. |

Reference checks to adapt include `src/routes/__tests__/auth-guard.test.ts`,
`src/lib/theme.test.tsx`, the DataTable behavior/search tests under
`src/components/ui/__tests__`, and
`tests/e2e/visual-regression.spec.ts`. Reference screenshots cover light/dark
desktop, tablet, and mobile views; product screenshots must cover the actual
workspace, login, table, and configuration flows. Test production deep links,
scope change, logout/cache teardown, keyboard/focus, and reduced motion with
real product data and authorization rather than importing cluster fixtures.

This is the P0-10 source and dependency inventory, not P0-10 acceptance. The
route/component implementation, exact license path, product screenshots,
browser journeys, and measured performance still need proof.
