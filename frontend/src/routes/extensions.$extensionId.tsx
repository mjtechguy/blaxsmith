import { createFileRoute } from "@tanstack/react-router";
import { ExtensionDetailPage } from "../extension-pages";

// Library › Extensions detail: versions, permissions, and templates, read only.
export const Route = createFileRoute("/extensions/$extensionId")({ component: Detail });

function Detail() {
  const { extensionId } = Route.useParams();
  return <ExtensionDetailPage key={extensionId} extensionId={extensionId} library />;
}
