import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { SourceEditor } from "../project-settings";
import { StatePanel } from "../ui";
import { useScope } from "../workspace-ui";
import { getProjectSource, projectSourceQueryKey } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/settings/source")({ component: SourceSection });

function SourceSection() {
  const { projectId } = Route.useParams();
  const { org, isAdmin, session } = useScope();
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: Boolean(org), queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  if (source.isPending || session.isPending) return <StatePanel kind="loading" title="Loading source" />;
  if (source.isError) return <StatePanel kind="error" title="Source unavailable" retry={() => void source.refetch()}>Source settings could not be loaded.</StatePanel>;
  if (!isAdmin) return <StatePanel kind="note" title="Source is read-only for your role">
    {source.data ? `Runs start from ${source.data.repositoryUrl}${source.data.ref ? ` at ${source.data.ref}` : " on its default branch"}. ` : "No source is configured yet. "}Only organization owners and admins change a project’s Git source.</StatePanel>;
  return <SourceEditor projectId={projectId} org={org} source={source.data} mode={{ kind: "settings" }} />;
}
