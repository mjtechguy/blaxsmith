import { createFileRoute } from "@tanstack/react-router";
import { RecipeLibraryPage } from "../recipe-pages";

// Library › Recipes: organization recipes readable by every member.
export const Route = createFileRoute("/recipes/")({ component: () => <RecipeLibraryPage library /> });
