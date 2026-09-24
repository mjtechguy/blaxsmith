import { createFileRoute } from "@tanstack/react-router";
import { RecipeLibraryPage } from "../recipe-pages";

export const Route = createFileRoute("/projects/$projectId/recipes/")({ component: Library });

function Library() {
  const { projectId } = Route.useParams();
  return <RecipeLibraryPage key={projectId} projectId={projectId} />;
}
