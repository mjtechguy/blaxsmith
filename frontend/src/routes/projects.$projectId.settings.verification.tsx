import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { VerificationEditor } from "../project-settings";
import { StatePanel } from "../ui";
import { useScope } from "../workspace-ui";
import { getProjectVerification, projectVerificationQueryKey } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/settings/verification")({ component: VerificationSection });

function VerificationSection() {
  const { projectId } = Route.useParams();
  const { org, isAdmin, session } = useScope();
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: Boolean(org), queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  if (verification.isPending || session.isPending) return <StatePanel kind="loading" title="Loading verification" />;
  if (verification.isError) return <StatePanel kind="error" title="Verification unavailable" retry={() => void verification.refetch()}>Checks could not be loaded.</StatePanel>;
  if (!isAdmin) return <StatePanel kind="note" title="Checks are read-only for your role">
    {verification.data ? `${verification.data.checks.map((c) => c.id).join(", ")} run before review. ` : "No checks are configured yet. "}Only organization owners and admins change verification.</StatePanel>;
  return <VerificationEditor projectId={projectId} org={org} current={verification.data} mode={{ kind: "settings" }} />;
}
