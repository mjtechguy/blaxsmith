import { createFileRoute, Outlet } from "@tanstack/react-router";

// A project's pages (overview, runs, recipes, connections, settings) are children
// of this layout; the sidebar's Project group follows the project in the URL.
export const Route = createFileRoute("/projects/$projectId")({ component: Outlet });
