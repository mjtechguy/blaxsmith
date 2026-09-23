import { createFileRoute } from "@tanstack/react-router";
import { ToolsPage } from "../tools-page";

export const Route = createFileRoute("/tools")({ component: ToolsPage });
