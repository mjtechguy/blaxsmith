import { createFileRoute } from "@tanstack/react-router";
import { RecipeLibraryPage } from "../recipe-pages";

export const Route = createFileRoute("/admin/recipes/")({ component: () => <RecipeLibraryPage /> });
