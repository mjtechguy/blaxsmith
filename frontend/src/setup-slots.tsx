// The setup features mounted in the shell's slots (see slots.tsx). The shell
// imports this module once, so every page and the route-tree tests see the
// same registrations.
import { useQuery } from "@tanstack/react-query";
import { currentSession, sessionQueryKey } from "./auth";
import { CommandPalette } from "./command-palette";
import { HealthLine, useConnections } from "./connection-ui";
import { OrgSetupChecklist, ProjectSetupChecklist } from "./setup-checklist";
import { registerSlot, type SlotProps } from "./slots";

const isAdmin = (role: string) => role === "owner" || role === "admin";

function TopbarCommand() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  return session.data ? <CommandPalette session={session.data} /> : null;
}

// Home and Operations show the organization checklist to the people who can act on it.
function OrgChecklist({ role }: { role: string }) {
  return isAdmin(role) ? <OrgSetupChecklist /> : null;
}

// Viewers cannot change a project, so its steps would only be noise.
function ProjectChecklist({ projectId, role }: SlotProps["project.checklist"]) {
  return role && role !== "viewer" ? <ProjectSetupChecklist projectId={projectId} /> : null;
}

// The detail page already loaded the connection list this reads from the cache.
function ConnectionHealth({ connectionId, scope, projectId = "" }: SlotProps["connection.health"]) {
  const inProject = scope === "project";
  const primary = useConnections(scope, inProject ? projectId : "");
  const available = useConnections("project_available", projectId, inProject);
  const connection = primary.data?.find((c) => c.id === connectionId) ?? available.data?.find((c) => c.id === connectionId);
  return connection ? <HealthLine connection={connection} /> : null;
}

registerSlot("topbar.command", TopbarCommand);
registerSlot("home.checklist", OrgChecklist);
registerSlot("admin.checklist", OrgChecklist);
registerSlot("project.checklist", ProjectChecklist);
registerSlot("connection.health", ConnectionHealth);
