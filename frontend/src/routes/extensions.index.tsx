import { createFileRoute } from "@tanstack/react-router";
import { ExtensionLibraryPage } from "../extension-pages";

// Library › Extensions: installed extensions granted to the member's projects.
// Read only; owners and admins install and grant under Admin › Extensions.
export const Route = createFileRoute("/extensions/")({ component: () => <ExtensionLibraryPage /> });
