import { createFileRoute, Outlet, useLocation } from "@tanstack/react-router";
import { SettingsLayout } from "../layouts";

export const Route = createFileRoute("/admin/settings")({ component: AdminSettings });

const adminSettingsSections = [
  { id: "github-app", label: "GitHub app", href: "/admin/settings/github-app" },
  { id: "connections", label: "Connections", href: "/admin/settings/connections" },
  { id: "policies", label: "Policies", href: "/admin/settings/policies", soon: true },
  { id: "retention", label: "Retention", href: "/admin/settings/retention", soon: true },
];

function AdminSettings() {
  const pathname = useLocation({ select: (l) => l.pathname.replace(/\/+$/, "") });
  const current = adminSettingsSections.find((s) => pathname === s.href)?.id ?? "github-app";
  return <SettingsLayout title="Organization settings" description="Organization-wide configuration. Each section saves on its own." sections={adminSettingsSections} current={current}>
    <Outlet />
  </SettingsLayout>;
}
