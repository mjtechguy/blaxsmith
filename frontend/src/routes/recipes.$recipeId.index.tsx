import { createFileRoute } from "@tanstack/react-router";
import { RecipeDetailPage } from "../recipe-pages";

// Library › Recipes detail: every member reads it; owners and admins also manage
// versions and grants here (the server enforces who may change what).
export const Route = createFileRoute("/recipes/$recipeId/")({ component: Detail });

function Detail() {
  const { recipeId } = Route.useParams();
  return <RecipeDetailPage key={recipeId} recipeId={recipeId} />;
}
