import { createFileRoute } from "@tanstack/react-router";
import { LoginPage } from "../login-page";

export const Route = createFileRoute("/login")({
  validateSearch: (search: Record<string, unknown>) => ({ next: search.next === "/tools" ? "/tools" : "/" }),
  component: LoginPage,
});
