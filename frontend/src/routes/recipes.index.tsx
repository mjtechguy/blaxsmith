import { createFileRoute } from "@tanstack/react-router";
import { RecipeLibraryPage } from "../recipe-pages";

// Library › Recipes: the one organization recipe list. Every member reads it;
// owners and admins also create and clone here.
export const Route = createFileRoute("/recipes/")({ component: () => <RecipeLibraryPage /> });
