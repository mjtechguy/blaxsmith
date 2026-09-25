import type { ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { Link } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";

// Tables and dashboards use the full content width (up to 1800 px); forms are
// constrained inside their own layouts (editor, settings, create flow).
export function PageShell({ children }: { children: ReactNode }) {
  return <div className="page-shell">{children}</div>;
}

// The topbar breadcrumbs carry the hierarchy, so `eyebrow` is accepted for
// older call sites but no longer rendered above the title.
export function PageHeader({ title, description, actions }: {
  eyebrow?: string; title: string; description?: string; actions?: ReactNode;
}) {
  return <div className="page-header"><div>
    <h1>{title}</h1>
    {description ? <p className="page-description">{description}</p> : null}
  </div>{actions ? <div className="page-actions">{actions}</div> : null}</div>;
}

// A detail page whose ID is unknown, malformed, or outside this organization:
// the page's one h1, what happened, and the way back to the list.
export function NotFoundPage({ title, back, children }: { title: string; back: { to: string; label: string }; children?: ReactNode }) {
  return <PageShell><div className="state-panel not-found" role="note">
    <h1>{title}</h1>
    <p>{children ?? "It may have been removed, or the link is wrong."}</p>
    <Link className="secondary-button" to={back.to as "/"}><ArrowLeft size={15} aria-hidden="true" /> {back.label}</Link>
  </div></PageShell>;
}

// The server answers NotFound for unknown IDs and InvalidArgument for malformed ones.
export const isMissing = (error: unknown) => {
  const code = ConnectError.from(error).code;
  return code === Code.NotFound || code === Code.InvalidArgument;
};
