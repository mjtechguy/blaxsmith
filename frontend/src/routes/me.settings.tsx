import { createFileRoute, Outlet, useLocation } from "@tanstack/react-router";
import { SettingsLayout } from "../layouts";

export const Route = createFileRoute("/me/settings")({ component: AccountSettings });

const sections = [
  { id: "profile", label: "Profile", href: "/me/settings/profile" },
  { id: "password", label: "Password", href: "/me/settings/password" },
  { id: "sessions", label: "Sessions", href: "/me/settings/sessions" },
  { id: "preferences", label: "Preferences", href: "/me/settings/preferences" },
  { id: "connections", label: "Connections", href: "/me/settings/connections" },
];

function AccountSettings() {
  const pathname = useLocation({ select: (l) => l.pathname.replace(/\/+$/, "") });
  const current = sections.find((s) => pathname === s.href)?.id ?? "profile";
  return <SettingsLayout title="Account settings" description="Your own account in this organization. Each section stands on its own." sections={sections} current={current}>
    <Outlet />
  </SettingsLayout>;
}
