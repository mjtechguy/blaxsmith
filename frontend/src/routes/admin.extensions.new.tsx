import { createFileRoute } from "@tanstack/react-router";
import { ExtensionInstallPage } from "../extension-pages";

type Search = { repo?: string; ref?: string };

export const Route = createFileRoute("/admin/extensions/new")({
  validateSearch: (search: Record<string, unknown>): Search => ({
    ...(typeof search.repo === "string" ? { repo: search.repo } : {}),
    ...(typeof search.ref === "string" ? { ref: search.ref } : {}),
  }),
  component: Install,
});

function Install() {
  const { repo, ref } = Route.useSearch();
  return <ExtensionInstallPage key={`${repo}@${ref}`} initial={{ repositoryUrl: repo, gitRef: ref }} />;
}
