import { createFileRoute } from "@tanstack/react-router";
import { ExtensionListPage } from "../extension-pages";

export const Route = createFileRoute("/admin/extensions/")({ component: () => <ExtensionListPage /> });
