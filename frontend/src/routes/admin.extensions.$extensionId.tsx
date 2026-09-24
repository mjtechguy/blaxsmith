import { createFileRoute } from "@tanstack/react-router";
import { ExtensionDetailPage } from "../extension-pages";

export const Route = createFileRoute("/admin/extensions/$extensionId")({ component: Detail });

function Detail() {
  const { extensionId } = Route.useParams();
  return <ExtensionDetailPage key={extensionId} extensionId={extensionId} />;
}
