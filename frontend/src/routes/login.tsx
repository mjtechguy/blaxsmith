import { createFileRoute } from "@tanstack/react-router";
import { safeNext } from "../nav";
import { LoginPage } from "../login-page";

export const Route = createFileRoute("/login")({
  validateSearch: (search: Record<string, unknown>) => ({ next: safeNext(search.next) }),
  component: LoginPage,
});
